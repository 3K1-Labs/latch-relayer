package main

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/amount"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

type scenario struct {
	name  string
	desc  string
	optIn bool // excluded from the default run
	run   func(*runner) error
}

var scenarios = []scenario{
	{name: "preflight", desc: "both services up, relayer auth on, user and depositor ready", run: scenarioPreflight},
	{name: "happy", desc: "deposit with memo is forwarded to a new smart account", run: scenarioHappy},
	{name: "nomemo", desc: "deposit without memo is swept to recovery", run: scenarioNoMemo},
	{name: "unknownmemo", desc: "deposit with a memo nobody issued is swept to recovery", run: scenarioUnknownMemo},
	{name: "authz", desc: "latch-api rejects unauthenticated, unregistered and cross-user calls", run: scenarioAuthz},
	{name: "walletauth", desc: "wallet-login user gets a 4xx, not a 500, on /v1/accounts (known bug)", optIn: true, run: scenarioWalletAuth},
	{name: "expired", desc: "deposit after the intent expires is swept, not forwarded (~6 min)", optIn: true, run: scenarioExpired},
}

func defaultScenarios() []string {
	var names []string
	for _, s := range scenarios {
		if !s.optIn {
			names = append(names, s.name)
		}
	}
	return names
}

func allScenarioNames() []string {
	names := make([]string, len(scenarios))
	for i, s := range scenarios {
		names[i] = s.name
	}
	return names
}

func pickScenarios(csv string) ([]scenario, error) {
	var picked []scenario
	for _, name := range strings.Split(csv, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := false
		for _, s := range scenarios {
			if s.name == name {
				picked = append(picked, s)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown scenario %q (have: %s)", name, strings.Join(allScenarioNames(), ", "))
		}
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("no scenarios selected")
	}
	return picked, nil
}

// ── Scenarios ────────────────────────────────────────────────────────────────

func scenarioPreflight(r *runner) error {
	if status, raw, err := r.anon.call(http.MethodGet, "/health", nil, nil); err != nil || status != http.StatusOK {
		return fmt.Errorf("latch-api /health: HTTP %d %v %s", status, err, trim(raw))
	}
	step("latch-api healthy")

	status, body, err := relayerGet(r.cfg.relayerURL+"/health", "")
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("relayer /health: HTTP %d %v %s", status, err, body)
	}
	step("relayer healthy")

	// Only latch-api should be able to use the relayer.
	if status, _, err := relayerGet(r.cfg.relayerURL+"/deposit/status/1", ""); err != nil || status != http.StatusUnauthorized {
		return fmt.Errorf("relayer without a key: got HTTP %d (%v), want 401", status, err)
	}
	step("relayer rejects requests without its API key")

	if _, err := r.loggedIn(); err != nil {
		return err
	}
	_, err = r.depositor()
	return err
}

func scenarioHappy(r *runner) error {
	user, err := r.loggedIn()
	if err != nil {
		return err
	}
	acct, err := r.smartAccount()
	if err != nil {
		return err
	}
	dep, err := r.depositor()
	if err != nil {
		return err
	}
	before, err := r.chain.sacBalance(r.ctx, dep.Address(), acct)
	if err != nil {
		return fmt.Errorf("read balance before: %w", err)
	}

	amt := r.uniqueAmount()
	in, err := createIntent(user, intentRequest{SmartAccountAddress: acct, ExpectedAmt: amt, ExternalID: r.externalID("happy")})
	if err != nil {
		return err
	}
	r.pool = in.PoolAddress
	if err := r.checkWiring(in.MemoID); err != nil {
		return err
	}

	memo, err := strconv.ParseUint(in.MemoID, 10, 64)
	if err != nil {
		return fmt.Errorf("memo_id %q is not a uint64: %w", in.MemoID, err)
	}
	hash, err := r.chain.pay(dep, in.PoolAddress, amt, txnbuild.MemoID(memo))
	if err != nil {
		return err
	}
	step("deposited %s XLM: %s", amt, txLink(hash))

	st, err := r.waitForIntent(user, in.MemoID, func(s depositStatus) bool {
		return s.Status == "completed" || s.Status == "failed"
	})
	if err != nil {
		return err
	}
	if st.Status != "completed" {
		return fmt.Errorf("intent ended %s: %s", st.Status, summarize(st))
	}
	// One inbound payment must produce exactly one forward.
	if len(st.Forwards) != 1 {
		return fmt.Errorf("want exactly 1 forward, got %d: %s", len(st.Forwards), summarize(st))
	}
	f := st.Forwards[0]
	if f.TxHash != hash {
		return fmt.Errorf("forward is for tx %s, want our deposit %s", f.TxHash, hash)
	}
	if f.Status != "done" || f.ForwardTx == nil {
		return fmt.Errorf("forward status %s, forward_tx %v", f.Status, f.ForwardTx)
	}
	ok, err := r.chain.txSucceeded(*f.ForwardTx)
	if err != nil {
		return fmt.Errorf("look up forward tx: %w", err)
	}
	if !ok {
		return fmt.Errorf("forward tx %s failed on chain", *f.ForwardTx)
	}
	step("forward tx succeeded: %s", txLink(*f.ForwardTx))

	// The ledger, not the relayer's word, is the final check.
	after, err := r.chain.sacBalance(r.ctx, dep.Address(), acct)
	if err != nil {
		return fmt.Errorf("read balance after: %w", err)
	}
	want, err := amount.ParseInt64(amt)
	if err != nil {
		return err
	}
	if after-before != want {
		return fmt.Errorf("smart account balance moved by %s XLM, want %s", amount.StringFromInt64(after-before), amt)
	}
	step("smart account balance %s → %s XLM", amount.StringFromInt64(before), amount.StringFromInt64(after))
	return nil
}

func scenarioNoMemo(r *runner) error {
	return r.depositAndExpectSweep("no memo", nil)
}

func scenarioUnknownMemo(r *runner) error {
	memo := rand.Uint64()
	return r.depositAndExpectSweep(fmt.Sprintf("unissued memo %d", memo), txnbuild.MemoID(memo))
}

func scenarioAuthz(r *runner) error {
	user, err := r.loggedIn()
	if err != nil {
		return err
	}
	acct, err := r.smartAccount()
	if err != nil {
		return err
	}

	status, raw, err := r.anon.call(http.MethodPost, "/v1/accounts/deposit-intent", intentRequest{SmartAccountAddress: acct}, nil)
	if err := expect(status, raw, err, "no token", http.StatusUnauthorized); err != nil {
		return err
	}

	// No mainnet check: a mainnet relayer is deployed, so latch-api serves
	// mainnet intents and this testnet-only script must not create one.

	stranger, err := randomContractAddress()
	if err != nil {
		return err
	}
	status, raw, err = user.call(http.MethodPost, "/v1/accounts/deposit-intent", intentRequest{SmartAccountAddress: stranger}, nil)
	if err := expect4xx(status, raw, err, "intent for an unregistered smart account"); err != nil {
		return err
	}

	if r.cfg.email2 == "" {
		step("E2E_EMAIL_2 unset, skipping the cross-user check")
		return nil
	}
	if r.user2 == nil {
		tok, err := emailLogin(r.anon, r.cfg.email2, r.stdin)
		if err != nil {
			return fmt.Errorf("log in %s: %w", r.cfg.email2, err)
		}
		r.user2 = newAPIClient(r.cfg.apiURL, tok)
	}
	in, err := createIntent(user, intentRequest{SmartAccountAddress: acct, ExternalID: r.externalID("authz")})
	if err != nil {
		return err
	}
	status, raw, err = r.user2.call(http.MethodGet, "/v1/accounts/deposit/status/"+in.MemoID, nil, nil)
	return expect4xx(status, raw, err, "another user's deposit status")
}

func scenarioWalletAuth(r *runner) error {
	tok, err := walletLogin(r.anon, keypair.MustRandom())
	if err != nil {
		return err
	}
	step("wallet sign-in ok")
	stranger, err := randomContractAddress()
	if err != nil {
		return err
	}
	wallet := newAPIClient(r.cfg.apiURL, tok)
	status, raw, err := wallet.call(http.MethodPost, "/v1/accounts/register", map[string]string{"smart_account_address": stranger}, nil)
	return expect4xx(status, raw, err, "wallet-token account register")
}

func scenarioExpired(r *runner) error {
	user, err := r.loggedIn()
	if err != nil {
		return err
	}
	acct, err := r.smartAccount()
	if err != nil {
		return err
	}
	dep, err := r.depositor()
	if err != nil {
		return err
	}
	// 300s is latch-api's minimum intent TTL.
	in, err := createIntent(user, intentRequest{SmartAccountAddress: acct, ExpiresIn: 300, ExternalID: r.externalID("expired")})
	if err != nil {
		return err
	}
	expiresAt, err := time.Parse(time.RFC3339, in.ExpiresAt)
	if err != nil {
		return fmt.Errorf("parse expires_at %q: %w", in.ExpiresAt, err)
	}
	for wait := time.Until(expiresAt) + 15*time.Second; wait > 0; wait = time.Until(expiresAt) + 15*time.Second {
		step("waiting %s for the intent to expire", wait.Round(time.Second))
		time.Sleep(min(wait, time.Minute))
	}

	amt := r.uniqueAmount()
	memo, err := strconv.ParseUint(in.MemoID, 10, 64)
	if err != nil {
		return err
	}
	since := time.Now().Add(-time.Minute)
	hash, err := r.chain.pay(dep, in.PoolAddress, amt, txnbuild.MemoID(memo))
	if err != nil {
		return err
	}
	step("deposited %s XLM after expiry: %s", amt, txLink(hash))

	st, err := r.waitForIntent(user, in.MemoID, func(s depositStatus) bool {
		for _, f := range s.Forwards {
			if f.TxHash == hash && f.Status != "pending" && f.Status != "pending_retry" {
				return true
			}
		}
		return false
	})
	if err != nil {
		return err
	}
	if st.Status == "completed" {
		return fmt.Errorf("expired intent was credited: %s", summarize(st))
	}
	return r.waitForSweep(in.PoolAddress, amt, since)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// uniqueAmount returns E2E_AMOUNT XLM plus a random 7-decimal fraction, so each
// deposit (and its sweep) can be matched on chain by amount alone.
func (r *runner) uniqueAmount() string {
	return fmt.Sprintf("%d.%07d", r.cfg.amountXLM, rand.IntN(9_000_000)+1_000_000)
}

func (r *runner) depositAndExpectSweep(label string, memo txnbuild.Memo) error {
	pool, err := r.poolAddress()
	if err != nil {
		return err
	}
	dep, err := r.depositor()
	if err != nil {
		return err
	}
	amt := r.uniqueAmount()
	since := time.Now().Add(-time.Minute) // slack for clock skew with the ledger
	hash, err := r.chain.pay(dep, pool, amt, memo)
	if err != nil {
		return err
	}
	step("deposited %s XLM with %s: %s", amt, label, txLink(hash))
	return r.waitForSweep(pool, amt, since)
}

func (r *runner) waitForSweep(pool, amt string, since time.Time) error {
	deadline := time.Now().Add(r.cfg.pollTimeout)
	for {
		p, err := r.chain.findOutgoingPayment(pool, amt, since)
		if err != nil {
			step("payments lookup: %v", err)
		}
		if p != nil {
			step("swept %s XLM to %s: %s", p.Amount, p.To, txLink(p.TransactionHash))
			if r.cfg.recoveryAddress != "" && p.To != r.cfg.recoveryAddress {
				step("note: that is not RECOVERY_ADDRESS from your local env (%s)", r.cfg.recoveryAddress)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no sweep of %s XLM out of %s within %s", amt, pool, r.cfg.pollTimeout)
		}
		time.Sleep(r.cfg.pollGap)
	}
}

func (r *runner) waitForIntent(api *apiClient, memoID string, done func(depositStatus) bool) (depositStatus, error) {
	deadline := time.Now().Add(r.cfg.pollTimeout)
	var last depositStatus
	lastLine := ""
	for {
		s, err := getStatus(api, memoID)
		if err != nil {
			step("status poll: %v", err)
		} else {
			last = s
			if line := summarize(s); line != lastLine {
				step("status: %s", line)
				lastLine = line
			}
			if done(s) {
				return s, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timed out after %s, last status: %s", r.cfg.pollTimeout, summarize(last))
		}
		time.Sleep(r.cfg.pollGap)
	}
}

// checkWiring confirms latch-api's RELAYER_URL is the relayer under test: the
// memo it just minted must be known to that relayer. Read-only.
func (r *runner) checkWiring(memoID string) error {
	if r.cfg.relayerAPIKey == "" {
		step("RELAYER_API_KEY unset, skipping the wiring check")
		return nil
	}
	status, body, err := relayerGet(r.cfg.relayerURL+"/deposit/status/"+memoID, r.cfg.relayerAPIKey)
	if err != nil {
		return fmt.Errorf("wiring check: %w", err)
	}
	switch status {
	case http.StatusOK:
		step("wiring ok: %s knows memo %s", r.cfg.relayerURL, memoID)
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("wiring check: RELAYER_API_KEY is not this relayer's key")
	default:
		return fmt.Errorf("wiring check: %s answered HTTP %d for memo %s, so latch-api is not using it: %s", r.cfg.relayerURL, status, memoID, body)
	}
}

func summarize(s depositStatus) string {
	parts := make([]string, len(s.Forwards))
	for i, f := range s.Forwards {
		parts[i] = f.Status
	}
	return fmt.Sprintf("intent=%s forwards=[%s]", s.Status, strings.Join(parts, ","))
}

func expect(status int, raw []byte, err error, label string, want int) error {
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if status != want {
		return fmt.Errorf("%s: got HTTP %d, want %d: %s", label, status, want, trim(raw))
	}
	step("%s → %d", label, status)
	return nil
}

func expect4xx(status int, raw []byte, err error, label string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if status/100 != 4 {
		return fmt.Errorf("%s: got HTTP %d, want 4xx: %s", label, status, trim(raw))
	}
	step("%s → %d", label, status)
	return nil
}

func randomContractAddress() (string, error) {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(rand.IntN(256))
	}
	return strkey.Encode(strkey.VersionByteContract, raw[:])
}

func relayerGet(url, apiKey string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, trim(raw), nil
}

// Command onramp_load drives concurrent on-ramps end to end and reports where
// the pipeline actually breaks.
//
// Each run: mint a session on latch-api's webapp route (the path that registers
// the memo with latch-relayer), confirm the relayer knows that memo, send a real
// testnet XLM payment to the pool carrying the memo as MEMO_ID, then poll the
// relayer until it forwards the deposit to the destination contract.
//
// Success is a forward reaching status "done". An HTTP 200 anywhere in the
// middle is not success — the whole point is whether money moves.
//
// Each run gets its OWN funded depositor account. Sharing one depositor would
// serialise every payment behind that account's sequence number, so the pool
// would never see concurrent arrivals and the run would measure nothing.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

type config struct {
	n           int
	cAddresses  []string
	apiBase     string
	relayerBase string
	relayerKey  string
	amount      string
	horizonURL  string
	creditWait  time.Duration
}

// result is one on-ramp attempt, start to finish.
type result struct {
	idx         int
	dest        string
	memoID      string
	poolAddress string
	intentID    string

	sessionMS  int64
	registered bool // relayer knew the memo before we paid
	payTxHash  string
	payMS      int64
	creditMS   int64 // deposit submitted -> relayer reports the forward done
	forwardTx  string
	retried    bool // landed via pending_retry rather than first attempt
	err        error
}

func main() {
	cfg := config{}
	flag.IntVar(&cfg.n, "n", 10, "concurrent on-ramps to run")
	cRaw := flag.String("c", "", "destination C-address(es), comma-separated (required)")
	flag.StringVar(&cfg.apiBase, "api", "http://localhost:8080", "latch-api base URL")
	flag.StringVar(&cfg.relayerBase, "relayer", "http://localhost:4000", "latch-relayer base URL")
	flag.StringVar(&cfg.amount, "amount", "1", "XLM per deposit")
	flag.StringVar(&cfg.horizonURL, "horizon", "https://horizon-testnet.stellar.org", "Horizon URL")
	flag.DurationVar(&cfg.creditWait, "credit-wait", 10*time.Minute, "how long to wait for credits")
	flag.Parse()

	cfg.relayerKey = os.Getenv("RELAYER_API_KEY")
	for _, a := range strings.Split(*cRaw, ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.cAddresses = append(cfg.cAddresses, a)
		}
	}
	if len(cfg.cAddresses) == 0 {
		fatal("-c (destination C-address) is required")
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(context.Background(), cfg); err != nil {
		fatal(err.Error())
	}
}

func run(ctx context.Context, cfg config) error {
	hz := &horizonclient.Client{HorizonURL: cfg.horizonURL}

	fmt.Printf("=== on-ramp load: n=%d amount=%s XLM relayer=%s ===\n", cfg.n, cfg.amount, cfg.relayerBase)
	fmt.Printf("    destinations (%d, round-robin): %s\n\n", len(cfg.cAddresses), strings.Join(shortAll(cfg.cAddresses), ", "))

	// The relayer sleeps when idle on a free-tier host, and its database
	// suspends separately. /health answers without touching the database, so it
	// is useless as a warm-up — a status lookup is the cheapest call that forces
	// a real query. Without this the first session eats the whole relayer budget
	// and 503s, which measures the host's cold start rather than our pipeline.
	fmt.Printf("[0/4] warming the relayer\n")
	if d, err := warmRelayer(ctx, cfg); err != nil {
		fmt.Printf("      WARNING: relayer still cold (%v) — expect 503s\n\n", err)
	} else {
		fmt.Printf("      warm after %s\n\n", d.Round(time.Millisecond))
	}

	// ── Stage 1: fund one depositor per run ──────────────────────────────────
	fmt.Printf("[1/4] funding %d depositor accounts via friendbot\n", cfg.n)
	t0 := time.Now()
	depositors, err := fundDepositors(ctx, hz, cfg.n)
	if err != nil {
		return fmt.Errorf("fund depositors: %w", err)
	}
	fmt.Printf("      funded %d in %s\n\n", len(depositors), time.Since(t0).Round(time.Second))

	// ── Stage 2: mint sessions concurrently ──────────────────────────────────
	fmt.Printf("[2/4] minting %d sessions concurrently on %s/api/on-ramp/session\n", cfg.n, cfg.apiBase)
	t0 = time.Now()
	results := make([]*result, cfg.n)
	var wg sync.WaitGroup
	for i := range cfg.n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = mintSession(ctx, cfg, i)
		}()
	}
	wg.Wait()
	sessionWall := time.Since(t0)
	minted := countWhere(results, func(r *result) bool { return r.err == nil })
	fmt.Printf("      %d/%d minted in %s\n\n", minted, cfg.n, sessionWall.Round(time.Millisecond))

	// ── Stage 3: confirm the relayer knows each memo, then pay ───────────────
	fmt.Printf("[3/4] verifying registration and submitting deposits\n")
	t0 = time.Now()
	for _, r := range results {
		if r.err == nil {
			r.registered = relayerKnowsMemo(ctx, cfg, r.memoID)
		}
	}
	wg = sync.WaitGroup{}
	for i, r := range results {
		if r.err != nil || !r.registered {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			payDeposit(ctx, cfg, hz, depositors[i], r)
		}()
	}
	wg.Wait()
	paid := countWhere(results, func(r *result) bool { return r.payTxHash != "" })
	fmt.Printf("      %d registered, %d deposits submitted in %s\n\n",
		countWhere(results, func(r *result) bool { return r.registered }), paid, time.Since(t0).Round(time.Second))

	// ── Stage 4: wait for the relayer to forward them ────────────────────────
	fmt.Printf("[4/4] waiting up to %s for credits\n", cfg.creditWait)
	awaitCredits(ctx, cfg, results)

	report(cfg, results, sessionWall)
	if countWhere(results, func(r *result) bool { return r.forwardTx != "" }) < cfg.n {
		return fmt.Errorf("not every deposit was credited")
	}
	return nil
}

// warmRelayer polls a status lookup until it answers quickly, so the run
// measures the pipeline rather than a cold start. Returns how long it took.
func warmRelayer(ctx context.Context, cfg config) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		probe := time.Now()
		// A memo that cannot exist: a 404 still proves the database answered.
		_, err := fetchDepositStatus(ctx, cfg, "1")
		elapsed := time.Since(probe)
		if err == nil || (elapsed < 3*time.Second && !isTimeout(err)) {
			return time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return time.Since(start), fmt.Errorf("still slow after %s", time.Since(start).Round(time.Second))
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// fundDepositors creates n keypairs and funds each from friendbot. Friendbot is
// rate-limited, so this runs with modest parallelism and tolerates retries.
func fundDepositors(ctx context.Context, hz *horizonclient.Client, n int) ([]*keypair.Full, error) {
	out := make([]*keypair.Full, n)
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			kp, err := keypair.Random()
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			for attempt := range 5 {
				if _, err = hz.Fund(kp.Address()); err == nil {
					out[i] = kp
					return
				}
				time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			}
			mu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf("friendbot fund %s: %w", kp.Address(), err)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

type sessionResp struct {
	IntentID    string `json:"intentId"`
	MemoID      string `json:"memoId"`
	PoolAddress string `json:"poolAddress"`
}

func mintSession(ctx context.Context, cfg config, idx int) *result {
	dest := cfg.cAddresses[idx%len(cfg.cAddresses)]
	r := &result{idx: idx, dest: dest}
	body, _ := json.Marshal(map[string]string{
		"destinationCAddress": dest,
		"fiatAmount":          "25",
		"fiatCode":            "USD",
	})

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.apiBase+"/api/on-ramp/session", bytes.NewReader(body))
	if err != nil {
		r.err = fmt.Errorf("build session request: %w", err)
		return r
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
	r.sessionMS = time.Since(start).Milliseconds()
	if err != nil {
		r.err = fmt.Errorf("call session: %w", err)
		return r
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		r.err = fmt.Errorf("session returned %d: %s", resp.StatusCode, truncate(string(raw), 160))
		return r
	}
	var out sessionResp
	if err := json.Unmarshal(raw, &out); err != nil {
		r.err = fmt.Errorf("decode session: %w", err)
		return r
	}
	r.memoID, r.poolAddress, r.intentID = out.MemoID, out.PoolAddress, out.IntentID
	return r
}

type depositStatus struct {
	MemoID   string `json:"memo_id"`
	Status   string `json:"status"`
	Forwards []struct {
		TxHash    string  `json:"tx_hash"`
		Status    string  `json:"status"`
		ForwardTx *string `json:"forward_tx"`
		Retries   int     `json:"retries"`
	} `json:"forwards"`
}

func fetchDepositStatus(ctx context.Context, cfg config, memoID string) (*depositStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.relayerBase+"/deposit/status/"+memoID, nil)
	if err != nil {
		return nil, fmt.Errorf("build status request: %w", err)
	}
	if cfg.relayerKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.relayerKey)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("call deposit status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deposit status returned %d", resp.StatusCode)
	}
	var out depositStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode deposit status: %w", err)
	}
	return &out, nil
}

// relayerKnowsMemo is the check that distinguishes a working on-ramp from the
// bug this branch fixed: a memo latch-api minted but never registered resolves
// to nothing here, and any deposit against it is swept to recovery.
func relayerKnowsMemo(ctx context.Context, cfg config, memoID string) bool {
	st, err := fetchDepositStatus(ctx, cfg, memoID)
	if err != nil {
		slog.Warn("memo not registered with relayer", "memo_id", memoID, "err", err)
		return false
	}
	return st.MemoID == memoID
}

func payDeposit(ctx context.Context, cfg config, hz *horizonclient.Client, dep *keypair.Full, r *result) {
	start := time.Now()
	acct, err := hz.AccountDetail(horizonclient.AccountRequest{AccountID: dep.Address()})
	if err != nil {
		r.err = fmt.Errorf("load depositor: %w", err)
		return
	}

	// MEMO_ID, never MEMO_TEXT: the relayer parses the tag as an integer, and a
	// text memo carrying the same digits is swept to recovery, never credited.
	memoVal, err := strconv.ParseUint(r.memoID, 10, 64)
	if err != nil {
		r.err = fmt.Errorf("parse memo %q: %w", r.memoID, err)
		return
	}
	memo := txnbuild.MemoID(memoVal)

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &acct,
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
		Memo:                 memo,
		Operations: []txnbuild.Operation{&txnbuild.Payment{
			Destination: r.poolAddress,
			Amount:      cfg.amount,
			Asset:       txnbuild.NativeAsset{},
		}},
	})
	if err != nil {
		r.err = fmt.Errorf("build payment: %w", err)
		return
	}
	if tx, err = tx.Sign(network.TestNetworkPassphrase, dep); err != nil {
		r.err = fmt.Errorf("sign payment: %w", err)
		return
	}

	sent, err := hz.SubmitTransaction(tx)
	r.payMS = time.Since(start).Milliseconds()
	if err != nil {
		r.err = fmt.Errorf("submit payment: %w", horizonclient.GetError(err))
		return
	}
	r.payTxHash = sent.Hash
}

// awaitCredits polls the relayer until every paid deposit reports a completed
// forward, or the budget runs out. Deliberately no back-off tuning: the shape
// of the wait is part of what we are measuring.
func awaitCredits(ctx context.Context, cfg config, results []*result) {
	paidAt := time.Now()
	deadline := paidAt.Add(cfg.creditWait)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		pending := 0
		for _, r := range results {
			if r.payTxHash == "" || r.forwardTx != "" {
				continue
			}
			st, err := fetchDepositStatus(ctx, cfg, r.memoID)
			if err != nil {
				pending++
				continue
			}
			done := false
			for _, f := range st.Forwards {
				if f.Status == "done" && f.ForwardTx != nil {
					r.forwardTx = *f.ForwardTx
					r.creditMS = time.Since(paidAt).Milliseconds()
					r.retried = f.Retries > 0
					done = true
					break
				}
			}
			if !done {
				pending++
			}
		}
		credited := countWhere(results, func(r *result) bool { return r.forwardTx != "" })
		fmt.Printf("      credited %d/%d (%s elapsed)\n", credited, cfg.n, time.Since(paidAt).Round(time.Second))
		if pending == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func report(cfg config, results []*result, sessionWall time.Duration) {
	var sessionMS, creditMS []int64
	var credited, registered, paid, retried int
	for _, r := range results {
		if r.err == nil {
			sessionMS = append(sessionMS, r.sessionMS)
		}
		if r.registered {
			registered++
		}
		if r.payTxHash != "" {
			paid++
		}
		if r.forwardTx != "" {
			credited++
			creditMS = append(creditMS, r.creditMS)
			if r.retried {
				retried++
			}
		}
	}

	fmt.Printf("\n=== results (n=%d) ===\n", cfg.n)
	fmt.Printf("sessions minted     %d/%d   wall %s\n", len(sessionMS), cfg.n, sessionWall.Round(time.Millisecond))
	fmt.Printf("memos registered    %d/%d\n", registered, cfg.n)
	fmt.Printf("deposits submitted  %d/%d\n", paid, cfg.n)
	fmt.Printf("deposits CREDITED   %d/%d\n", credited, cfg.n)
	fmt.Printf("  of which retried  %d\n", retried)

	if len(sessionMS) > 0 {
		fmt.Printf("session latency     p50 %dms  p95 %dms  max %dms\n",
			pct(sessionMS, 50), pct(sessionMS, 95), pct(sessionMS, 100))
	}
	if len(creditMS) > 0 {
		fmt.Printf("deposit->credit     p50 %s  p95 %s  max %s\n",
			ms(pct(creditMS, 50)), ms(pct(creditMS, 95)), ms(pct(creditMS, 100)))
		total := float64(pct(creditMS, 100)) / 1000
		if total > 0 {
			fmt.Printf("sustained rate      %.2f credits/sec (%.1f/min)\n",
				float64(credited)/total, float64(credited)/total*60)
		}
	}

	fmt.Printf("\nfailures:\n")
	any := false
	for _, r := range results {
		switch {
		case r.err != nil:
			fmt.Printf("  #%d %v\n", r.idx, r.err)
			any = true
		case !r.registered:
			fmt.Printf("  #%d memo %s NOT registered with relayer\n", r.idx, r.memoID)
			any = true
		case r.forwardTx == "":
			fmt.Printf("  #%d memo %s paid (%s) but never credited\n", r.idx, r.memoID, r.payTxHash)
			any = true
		}
	}
	if !any {
		fmt.Printf("  none\n")
	}
}

func countWhere(rs []*result, pred func(*result) bool) int {
	n := 0
	for _, r := range rs {
		if r != nil && pred(r) {
			n++
		}
	}
	return n
}

func pct(v []int64, p int) int64 {
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (len(s) - 1) * p / 100
	return s[idx]
}

func ms(v int64) string { return (time.Duration(v) * time.Millisecond).Round(time.Second).String() }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fatal(msg string) {
	fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	os.Exit(1)
}

func short(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "\u2026" + a[len(a)-4:]
}

func shortAll(as []string) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = short(a)
	}
	return out
}

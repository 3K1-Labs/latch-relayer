// Command burst fires a burst of deposits at the deployed deposit relayer, all
// landing in the same ledger or two, and measures how long the relayer takes to
// forward every one of them.
//
// It skips latch-api on purpose: intents are minted directly on the relayer
// with RELAYER_API_KEY, so latch-api's 5-intents-per-minute limit does not cap
// the burst. Each deposit is sent from its own depositor account, derived from
// the master depositor and funded in batches of 100 by it, so no friendbot
// limits and no shared sequence number serialise the burst.
//
//	make burst N=50 C=CB...
//
// Latency is measured on chain: from the ledger the deposit closed in to the
// ledger its forward closed in, so polling cadence does not blur the numbers.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/amount"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

const (
	defaultRelayerURL = "https://latch-relayer-zy93.onrender.com"
	defaultHorizonURL = "https://horizon-testnet.stellar.org"
	opsPerTx          = 100 // Stellar's per-transaction operation limit
)

type config struct {
	n           int
	amount      string
	cAddress    string
	relayerURL  string
	relayerKey  string
	horizonURL  string
	masterSeed  string
	timeout     time.Duration
	concurrency int
	csvPath     string
}

// deposit is one burst member, start to finish.
type deposit struct {
	idx int
	kp  *keypair.Full
	seq int64

	memoID    string
	pool      string
	intentDur time.Duration

	payHash   string
	payLedger int32
	payClose  time.Time

	fwdStatus string // done | failed | timeout
	fwdTx     string
	fwdLedger int32
	fwdClose  time.Time

	err error
}

func main() {
	_ = godotenv.Load("e2e.env")
	_ = godotenv.Load(".env")

	cfg := config{}
	flag.IntVar(&cfg.n, "n", 12, "deposits in the burst")
	flag.StringVar(&cfg.amount, "amount", "1", "XLM per deposit")
	flag.StringVar(&cfg.cAddress, "c", os.Getenv("BURST_C_ADDRESS"), "destination smart account (C-address)")
	flag.DurationVar(&cfg.timeout, "timeout", 20*time.Minute, "how long to wait for every forward")
	flag.IntVar(&cfg.concurrency, "concurrency", 20, "parallel HTTP calls for intents, polling and lookups")
	flag.StringVar(&cfg.csvPath, "csv", "", "write one row per deposit to this file")
	flag.Parse()

	cfg.relayerURL = strings.TrimRight(envOr("RELAYER_URL", defaultRelayerURL), "/")
	cfg.relayerKey = os.Getenv("RELAYER_API_KEY")
	cfg.horizonURL = envOr("E2E_HORIZON_URL", defaultHorizonURL)
	cfg.masterSeed = envOr("E2E_DEPOSITOR_SEED", os.Getenv("DEPOSITOR_PRIVATE_KEY"))

	switch {
	case !strings.HasPrefix(cfg.cAddress, "C"):
		fatalf("-c (or BURST_C_ADDRESS) must be a smart account C-address")
	case cfg.relayerKey == "":
		fatalf("RELAYER_API_KEY is required: intents are minted directly on the relayer")
	case cfg.masterSeed == "":
		fatalf("E2E_DEPOSITOR_SEED or DEPOSITOR_PRIVATE_KEY is required to fund the burst depositors")
	case cfg.n < 1:
		fatalf("-n must be at least 1")
	}
	if _, err := amount.ParseInt64(cfg.amount); err != nil {
		fatalf("-amount: %v", err)
	}

	if err := run(context.Background(), cfg); err != nil {
		fatalf("%v", err)
	}
}

func run(ctx context.Context, cfg config) error {
	hz := &horizonclient.Client{
		HorizonURL: cfg.horizonURL,
		HTTP: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
			MaxIdleConnsPerHost: 100,
		}},
	}
	root, err := hz.Root()
	if err != nil {
		return fmt.Errorf("horizon root: %w", err)
	}
	if root.NetworkPassphrase != network.TestNetworkPassphrase {
		return fmt.Errorf("safety check: horizon is on %q, not testnet", root.NetworkPassphrase)
	}
	master, err := keypair.ParseFull(cfg.masterSeed)
	if err != nil {
		return fmt.Errorf("parse master depositor seed: %w", err)
	}
	rl := &relayer{base: cfg.relayerURL, key: cfg.relayerKey, http: &http.Client{Timeout: 90 * time.Second}}
	runID := time.Now().UTC().Format("20060102T150405")

	fmt.Printf("=== relayer burst: n=%d amount=%s XLM ===\n", cfg.n, cfg.amount)
	fmt.Printf("relayer: %s\ndest:    %s\nrun id:  burst-%s\n\n", cfg.relayerURL, cfg.cAddress, runID)

	// ── 0. Warm up ───────────────────────────────────────────────────────────
	// A status lookup forces a real database query, which /health does not, so
	// a sleeping host and a suspended database are both awake before the clock
	// starts.
	fmt.Println("[0/5] warming the relayer")
	t := time.Now()
	if _, err := rl.status(ctx, "1"); err != nil && !errors.Is(err, errNotFound) {
		return fmt.Errorf("warm-up: %w", err)
	}
	before, _ := rl.metrics(ctx)
	fmt.Printf("      warm after %s\n\n", time.Since(t).Round(time.Millisecond))

	// ── 1. Depositors ────────────────────────────────────────────────────────
	fmt.Printf("[1/5] preparing %d depositor accounts\n", cfg.n)
	t = time.Now()
	deps := make([]*deposit, cfg.n)
	for i := range deps {
		deps[i] = &deposit{idx: i, kp: deriveDepositor(master, i)}
	}
	if err := fundDepositors(hz, master, deps, cfg); err != nil {
		return err
	}
	fmt.Printf("      ready in %s\n\n", time.Since(t).Round(time.Millisecond))

	// ── 2. Intents ───────────────────────────────────────────────────────────
	fmt.Printf("[2/5] minting %d intents on the relayer\n", cfg.n)
	t = time.Now()
	parallel(cfg.concurrency, deps, func(d *deposit) {
		start := time.Now()
		in, err := rl.createIntent(ctx, cfg.cAddress, cfg.amount, fmt.Sprintf("burst-%s-%d", runID, d.idx))
		d.intentDur = time.Since(start)
		if err != nil {
			d.err = fmt.Errorf("create intent: %w", err)
			return
		}
		d.memoID, d.pool = in.MemoID, in.PoolAddress
	})
	ok := count(deps, func(d *deposit) bool { return d.memoID != "" })
	fmt.Printf("      %d/%d in %s (p50 %s, p95 %s)\n", ok, cfg.n, time.Since(t).Round(time.Millisecond),
		pct(durs(deps, func(d *deposit) (time.Duration, bool) { return d.intentDur, d.memoID != "" }), 50),
		pct(durs(deps, func(d *deposit) (time.Duration, bool) { return d.intentDur, d.memoID != "" }), 95))
	fmt.Printf("      pools handed out: %s\n\n", strings.Join(distinct(deps, func(d *deposit) string { return d.pool }), ", "))

	// ── 3. Fire ──────────────────────────────────────────────────────────────
	// Build and sign everything first so the submissions leave together.
	fmt.Printf("[3/5] firing %d deposits at once\n", ok)
	signed := make(map[int]*txnbuild.Transaction)
	for _, d := range deps {
		if d.err != nil {
			continue
		}
		tx, err := buildDeposit(d, cfg.amount)
		if err != nil {
			d.err = fmt.Errorf("build deposit: %w", err)
			continue
		}
		signed[d.idx] = tx
	}
	t = time.Now()
	var wg sync.WaitGroup
	for _, d := range deps {
		tx, ok := signed[d.idx]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(d *deposit, tx *txnbuild.Transaction) {
			defer wg.Done()
			res, err := hz.SubmitTransaction(tx)
			if err != nil {
				d.err = fmt.Errorf("submit deposit: %w", describeHorizonErr(err))
				return
			}
			d.payHash, d.payLedger = res.Hash, res.Ledger
		}(d, tx)
	}
	wg.Wait()
	landed := count(deps, func(d *deposit) bool { return d.payHash != "" })
	ledgers := distinct(deps, func(d *deposit) string {
		if d.payLedger == 0 {
			return ""
		}
		return strconv.Itoa(int(d.payLedger))
	})
	fmt.Printf("      %d/%d landed in %s across %d ledger(s): %s\n\n", landed, ok, time.Since(t).Round(time.Millisecond), len(ledgers), strings.Join(ledgers, ", "))

	// ── 4. Wait for forwards ─────────────────────────────────────────────────
	fmt.Printf("[4/5] waiting for forwards (timeout %s)\n", cfg.timeout)
	t = time.Now()
	deadline := t.Add(cfg.timeout)
	lastReport := time.Time{}
	for {
		pending := filter(deps, func(d *deposit) bool { return d.payHash != "" && d.fwdStatus == "" })
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			for _, d := range pending {
				d.fwdStatus = "timeout"
			}
			break
		}
		parallel(cfg.concurrency, pending, func(d *deposit) {
			st, err := rl.status(ctx, d.memoID)
			if err != nil {
				return // transient; try again next round
			}
			for _, f := range st.Forwards {
				if f.TxHash != d.payHash {
					continue
				}
				switch {
				case f.Status == "done" && f.ForwardTx != nil:
					d.fwdStatus, d.fwdTx = "done", *f.ForwardTx
				case f.Status == "failed":
					d.fwdStatus = "failed"
					d.err = fmt.Errorf("forward failed")
				}
			}
		})
		if time.Since(lastReport) >= 15*time.Second {
			done := count(deps, func(d *deposit) bool { return d.fwdStatus == "done" })
			fmt.Printf("      %6s  %d/%d forwarded\n", time.Since(t).Round(time.Second), done, landed)
			lastReport = time.Now()
		}
		time.Sleep(3 * time.Second)
	}
	fmt.Println()

	// ── 5. On-chain timings ──────────────────────────────────────────────────
	fmt.Println("[5/5] reading ledger close times")
	parallel(cfg.concurrency/2+1, deps, func(d *deposit) {
		if d.payHash != "" {
			if tx, err := hz.TransactionDetail(d.payHash); err == nil {
				d.payClose = tx.LedgerCloseTime
			}
		}
		if d.fwdTx != "" {
			if tx, err := hz.TransactionDetail(d.fwdTx); err == nil {
				d.fwdClose, d.fwdLedger = tx.LedgerCloseTime, tx.Ledger
			}
		}
	})
	after, _ := rl.metrics(ctx)

	report(cfg, deps, before, after)
	if cfg.csvPath != "" {
		if err := writeCSV(cfg.csvPath, deps); err != nil {
			return err
		}
		fmt.Printf("\nper-deposit rows written to %s\n", cfg.csvPath)
	}
	if count(deps, func(d *deposit) bool { return d.fwdStatus == "done" }) != cfg.n {
		os.Exit(1)
	}
	return nil
}

// ── Depositors ───────────────────────────────────────────────────────────────

// deriveDepositor returns the i-th burst depositor. Derived rather than random
// so later runs reuse (and only top up) the accounts earlier runs created.
func deriveDepositor(master *keypair.Full, i int) *keypair.Full {
	seed := sha256.Sum256([]byte(fmt.Sprintf("latch-burst-depositor:%s:%d", master.Seed(), i)))
	kp, err := keypair.FromRawSeed(seed)
	if err != nil {
		panic(err) // any 32 bytes are a valid ed25519 seed
	}
	return kp
}

// fundDepositors creates missing depositors and tops up low ones from master,
// 100 operations per transaction, then loads every depositor's sequence.
func fundDepositors(hz *horizonclient.Client, master *keypair.Full, deps []*deposit, cfg config) error {
	amt, _ := amount.ParseInt64(cfg.amount)
	target := amt + 3*amount.One // deposit + 1 XLM reserve + headroom for fees
	floor := amt + amount.One + amount.One/10

	var ops []txnbuild.Operation
	var mu sync.Mutex
	var loadErr error
	parallel(cfg.concurrency, deps, func(d *deposit) {
		acct, err := hz.AccountDetail(horizonclient.AccountRequest{AccountID: d.kp.Address()})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if hzErr, ok := err.(*horizonclient.Error); ok && hzErr.Response.StatusCode == http.StatusNotFound {
				ops = append(ops, &txnbuild.CreateAccount{Destination: d.kp.Address(), Amount: amount.StringFromInt64(target)})
				return
			}
			loadErr = fmt.Errorf("load depositor %d: %w", d.idx, err)
			return
		}
		bal, _ := acct.GetNativeBalance()
		have, _ := amount.ParseInt64(bal)
		if have < floor {
			ops = append(ops, &txnbuild.Payment{Destination: d.kp.Address(), Amount: amount.StringFromInt64(target - have), Asset: txnbuild.NativeAsset{}})
		}
		d.seq = acct.Sequence
	})
	if loadErr != nil {
		return loadErr
	}

	if len(ops) > 0 {
		fmt.Printf("      funding %d depositor(s) from %s\n", len(ops), master.Address())
		masterAcct, err := hz.AccountDetail(horizonclient.AccountRequest{AccountID: master.Address()})
		if err != nil {
			return fmt.Errorf("load master depositor: %w", err)
		}
		for start := 0; start < len(ops); start += opsPerTx {
			batch := ops[start:min(start+opsPerTx, len(ops))]
			tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
				SourceAccount:        &masterAcct,
				IncrementSequenceNum: true,
				Operations:           batch,
				BaseFee:              txnbuild.MinBaseFee,
				Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
			})
			if err != nil {
				return err
			}
			if tx, err = tx.Sign(network.TestNetworkPassphrase, master); err != nil {
				return err
			}
			if _, err := hz.SubmitTransaction(tx); err != nil {
				return fmt.Errorf("fund depositors: %w", describeHorizonErr(err))
			}
		}
		// Newly created accounts start at a ledger-derived sequence; read it.
		parallel(cfg.concurrency, deps, func(d *deposit) {
			if d.seq != 0 {
				return
			}
			acct, err := hz.AccountDetail(horizonclient.AccountRequest{AccountID: d.kp.Address()})
			if err != nil {
				d.err = fmt.Errorf("reload depositor: %w", err)
				return
			}
			d.seq = acct.Sequence
		})
	}
	return nil
}

func buildDeposit(d *deposit, amt string) (*txnbuild.Transaction, error) {
	memo, err := strconv.ParseUint(d.memoID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("memo_id %q: %w", d.memoID, err)
	}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: d.kp.Address(), Sequence: d.seq},
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{&txnbuild.Payment{Destination: d.pool, Amount: amt, Asset: txnbuild.NativeAsset{}}},
		Memo:                 txnbuild.MemoID(memo),
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		return nil, err
	}
	return tx.Sign(network.TestNetworkPassphrase, d.kp)
}

// ── Relayer client ───────────────────────────────────────────────────────────

var errNotFound = errors.New("not found")

type relayer struct {
	base, key string
	http      *http.Client
}

type intentResp struct {
	MemoID      string `json:"memo_id"`
	PoolAddress string `json:"pool_address"`
}

type statusResp struct {
	Status   string `json:"status"`
	Forwards []struct {
		TxHash    string  `json:"tx_hash"`
		Status    string  `json:"status"`
		ForwardTx *string `json:"forward_tx"`
	} `json:"forwards"`
}

// do sends one request, honouring the relayer's 429 Retry-After a few times
// so the tool measures forwarding rather than its own rate limit.
func (r *relayer) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, r.base+path, rdr)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+r.key)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := r.http.Do(req)
		if err != nil {
			return 0, nil, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			time.Sleep(time.Duration(max(wait, 1)) * time.Second)
			continue
		}
		return resp.StatusCode, raw, nil
	}
}

func (r *relayer) createIntent(ctx context.Context, cAddress, amt, externalID string) (intentResp, error) {
	body, _ := json.Marshal(map[string]any{"c_address": cAddress, "expected_amt": amt, "expires_in": 3600, "external_id": externalID})
	status, raw, err := r.do(ctx, http.MethodPost, "/intents", body)
	if err != nil {
		return intentResp{}, err
	}
	if status != http.StatusCreated {
		return intentResp{}, fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(raw)))
	}
	var in intentResp
	return in, json.Unmarshal(raw, &in)
}

func (r *relayer) status(ctx context.Context, memoID string) (statusResp, error) {
	status, raw, err := r.do(ctx, http.MethodGet, "/deposit/status/"+memoID, nil)
	if err != nil {
		return statusResp{}, err
	}
	if status == http.StatusNotFound {
		return statusResp{}, errNotFound
	}
	if status != http.StatusOK {
		return statusResp{}, fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(raw)))
	}
	var s statusResp
	return s, json.Unmarshal(raw, &s)
}

// metrics reads the relayer's relayer_* counters and gauges, keyed by the
// full series name including labels.
func (r *relayer) metrics(ctx context.Context) (map[string]float64, error) {
	status, raw, err := r.do(ctx, http.MethodGet, "/metrics", nil)
	if err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("metrics: HTTP %d %v", status, err)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "relayer_") {
			continue
		}
		i := strings.LastIndex(line, " ")
		if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
			out[line[:i]] = v
		}
	}
	return out, nil
}

// ── Report ───────────────────────────────────────────────────────────────────

func report(cfg config, deps []*deposit, before, after map[string]float64) {
	done := filter(deps, func(d *deposit) bool { return d.fwdStatus == "done" && !d.fwdClose.IsZero() && !d.payClose.IsZero() })
	fmt.Println("\n=== results ===")
	fmt.Printf("forwards:  done %d  failed %d  timeout %d  never sent %d  (of %d)\n",
		count(deps, func(d *deposit) bool { return d.fwdStatus == "done" }),
		count(deps, func(d *deposit) bool { return d.fwdStatus == "failed" }),
		count(deps, func(d *deposit) bool { return d.fwdStatus == "timeout" }),
		count(deps, func(d *deposit) bool { return d.payHash == "" }),
		cfg.n)

	if len(done) > 0 {
		credit := durs(done, func(d *deposit) (time.Duration, bool) { return d.fwdClose.Sub(d.payClose), true })
		fmt.Printf("credit time (deposit ledger → forward ledger):  p50 %s  p95 %s  max %s\n", pct(credit, 50), pct(credit, 95), pct(credit, 100))

		first, last := done[0].payClose, done[0].fwdClose
		perLedger := map[int32]int{}
		for _, d := range done {
			if d.payClose.Before(first) {
				first = d.payClose
			}
			if d.fwdClose.After(last) {
				last = d.fwdClose
			}
			perLedger[d.fwdLedger]++
		}
		drain := last.Sub(first)
		rate := float64(len(done)) / max(drain.Minutes(), 1.0/60)
		fmt.Printf("drain:     %s from first deposit to last forward  →  %.1f forwards/min\n", drain.Round(time.Second), rate)

		maxPer := 0
		for _, c := range perLedger {
			maxPer = max(maxPer, c)
		}
		pools := distinct(deps, func(d *deposit) string { return d.pool })
		fmt.Printf("ledgers:   forwards spread over %d ledgers, at most %d in one ledger (%d pool(s))\n", len(perLedger), maxPer, len(pools))
	}

	if before != nil && after != nil {
		fmt.Println("relayer metrics (change during run):")
		keys := make([]string, 0, len(after))
		for k := range after {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.HasSuffix(k, "_bucket") || strings.Contains(k, "_bucket{") {
				continue
			}
			if delta := after[k] - before[k]; delta != 0 || strings.Contains(k, "depth") {
				fmt.Printf("  %-60s %+g (now %g)\n", k, delta, after[k])
			}
		}
	}

	var failures []string
	for _, d := range deps {
		if d.err != nil {
			failures = append(failures, fmt.Sprintf("  #%d memo=%s: %v", d.idx, d.memoID, d.err))
		}
	}
	if len(failures) > 0 {
		fmt.Println("errors:")
		for _, f := range failures[:min(len(failures), 20)] {
			fmt.Println(f)
		}
		if len(failures) > 20 {
			fmt.Printf("  … and %d more\n", len(failures)-20)
		}
	}
}

func writeCSV(path string, deps []*deposit) error {
	var b strings.Builder
	b.WriteString("idx,memo_id,pool,intent_ms,deposit_tx,deposit_ledger,deposit_close,forward_status,forward_tx,forward_ledger,forward_close,credit_s,error\n")
	for _, d := range deps {
		credit := ""
		if !d.fwdClose.IsZero() && !d.payClose.IsZero() {
			credit = strconv.FormatFloat(d.fwdClose.Sub(d.payClose).Seconds(), 'f', 0, 64)
		}
		errText := ""
		if d.err != nil {
			errText = strings.ReplaceAll(d.err.Error(), ",", ";")
		}
		fmt.Fprintf(&b, "%d,%s,%s,%d,%s,%d,%s,%s,%s,%d,%s,%s,%s\n",
			d.idx, d.memoID, d.pool, d.intentDur.Milliseconds(), d.payHash, d.payLedger, stamp(d.payClose),
			d.fwdStatus, d.fwdTx, d.fwdLedger, stamp(d.fwdClose), credit, errText)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// parallel runs fn over items with at most n in flight.
func parallel(n int, items []*deposit, fn func(*deposit)) {
	sem := make(chan struct{}, max(n, 1))
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(it *deposit) {
			defer func() { <-sem; wg.Done() }()
			fn(it)
		}(it)
	}
	wg.Wait()
}

func filter(deps []*deposit, keep func(*deposit) bool) []*deposit {
	var out []*deposit
	for _, d := range deps {
		if keep(d) {
			out = append(out, d)
		}
	}
	return out
}

func count(deps []*deposit, keep func(*deposit) bool) int { return len(filter(deps, keep)) }

func distinct(deps []*deposit, key func(*deposit) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range deps {
		if k := key(d); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func durs(deps []*deposit, get func(*deposit) (time.Duration, bool)) []time.Duration {
	var out []time.Duration
	for _, d := range deps {
		if v, ok := get(d); ok {
			out = append(out, v)
		}
	}
	return out
}

// pct returns the p-th percentile (nearest rank); p=100 is the maximum.
func pct(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := (p*len(s)+99)/100 - 1
	return s[min(max(i, 0), len(s)-1)].Round(time.Millisecond)
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func describeHorizonErr(err error) error {
	if herr, ok := err.(*horizonclient.Error); ok {
		if codes, cerr := herr.ResultCodes(); cerr == nil {
			return fmt.Errorf("%w (tx=%s ops=%v)", err, codes.TransactionCode, codes.OperationCodes)
		}
	}
	return err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "burst: "+format+"\n", args...)
	os.Exit(1)
}

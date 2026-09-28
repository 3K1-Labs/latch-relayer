// Command e2e_live runs end-to-end deposit tests against the deployed
// latch-api and latch-relayer on testnet, driving everything through
// latch-api exactly as latch-mobile does: log in, deploy a smart account,
// register it, open a deposit intent, pay the pool, and watch the relayer
// forward the funds.
//
//	make e2e-live                       # default scenarios
//	make e2e-live RUN=happy,nomemo      # a subset
//	make e2e-live RUN=expired           # opt-in, ~6 minutes
//
// Configuration comes from e2e.env, then .env (see e2e.env.example).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/keypair"
)

const (
	defaultAPIURL     = "https://latch-api-4cvn.onrender.com"
	defaultRelayerURL = "https://latch-relayer-zy93.onrender.com"
	defaultHorizonURL = "https://horizon-testnet.stellar.org"
	defaultRPCURL     = "https://soroban-testnet.stellar.org"
)

type config struct {
	apiURL, relayerURL   string
	horizonURL, rpcURL   string
	relayerAPIKey        string // optional: read-only wiring check only
	email, email2        string // email2 optional: cross-user check
	depositorSeed        string // optional: friendbot-funded when empty
	recoveryAddress      string // optional: where sweeps are expected to land
	amountXLM            int    // whole-XLM part of every deposit
	pollTimeout, pollGap time.Duration
}

func loadConfig() (config, error) {
	// godotenv never overrides a variable that is already set, so e2e.env wins
	// over .env and the shell wins over both.
	_ = godotenv.Load("e2e.env")
	_ = godotenv.Load(".env")

	c := config{
		apiURL:          strings.TrimRight(envOr("LATCH_API_URL", defaultAPIURL), "/"),
		relayerURL:      strings.TrimRight(envOr("RELAYER_URL", defaultRelayerURL), "/"),
		horizonURL:      envOr("E2E_HORIZON_URL", defaultHorizonURL),
		rpcURL:          envOr("E2E_RPC_URL", defaultRPCURL),
		relayerAPIKey:   os.Getenv("RELAYER_API_KEY"),
		email:           os.Getenv("E2E_EMAIL"),
		email2:          os.Getenv("E2E_EMAIL_2"),
		depositorSeed:   envOr("E2E_DEPOSITOR_SEED", os.Getenv("DEPOSITOR_PRIVATE_KEY")),
		recoveryAddress: os.Getenv("RECOVERY_ADDRESS"),
		pollGap:         3 * time.Second,
	}
	amt, err := strconv.Atoi(envOr("E2E_AMOUNT", "2"))
	if err != nil || amt < 1 {
		return c, fmt.Errorf("E2E_AMOUNT must be a whole number of XLM >= 1")
	}
	c.amountXLM = amt
	if c.email == "" {
		return c, fmt.Errorf("E2E_EMAIL is required: deposit endpoints only accept email-login users")
	}
	return c, nil
}

func main() {
	run := flag.String("run", strings.Join(defaultScenarios(), ","), "comma-separated scenarios: "+strings.Join(allScenarioNames(), ", "))
	timeout := flag.Duration("timeout", 3*time.Minute, "how long to wait for each forward or sweep")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		fatalf("config: %v", err)
	}
	cfg.pollTimeout = *timeout

	selected, err := pickScenarios(*run)
	if err != nil {
		fatalf("%v", err)
	}

	ctx := context.Background()
	chain := newChain(cfg.horizonURL, cfg.rpcURL)
	// Refuse to touch anything unless both chain endpoints are testnet: this
	// script deploys accounts and moves funds.
	if err := chain.requireTestnet(ctx); err != nil {
		fatalf("safety check: %v", err)
	}

	r := &runner{
		ctx:   ctx,
		cfg:   cfg,
		chain: chain,
		anon:  newAPIClient(cfg.apiURL, ""),
		stdin: bufio.NewReader(os.Stdin),
		runID: time.Now().UTC().Format("20060102T150405"),
	}

	fmt.Println("=== latch live E2E (testnet) ===")
	fmt.Printf("latch-api: %s\nrelayer:   %s\nrun id:    e2e-%s\n", cfg.apiURL, cfg.relayerURL, r.runID)

	var results []result
	for _, s := range selected {
		fmt.Printf("\n── %s: %s\n", s.name, s.desc)
		start := time.Now()
		err := s.run(r)
		results = append(results, result{name: s.name, err: err, took: time.Since(start)})
		if err != nil {
			fmt.Printf("  ✗ FAIL: %v\n", err)
		} else {
			fmt.Println("  ✓ PASS")
		}
	}

	fmt.Println("\n=== summary ===")
	failed := 0
	for _, res := range results {
		mark := "PASS"
		if res.err != nil {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("  %-4s  %-12s %6.1fs\n", mark, res.name, res.took.Seconds())
	}
	if failed > 0 {
		fmt.Printf("%d of %d scenarios failed\n", failed, len(results))
		os.Exit(1)
	}
	fmt.Printf("all %d scenarios passed\n", len(results))
}

type result struct {
	name string
	err  error
	took time.Duration
}

// runner holds what scenarios share. The logged-in users, the depositor and
// the smart account are created on first use, so running one scenario only
// pays for what it needs.
type runner struct {
	ctx   context.Context
	cfg   config
	chain *chain
	anon  *apiClient
	stdin *bufio.Reader
	runID string

	user, user2 *apiClient
	depositorKP *keypair.Full
	account     string // smart account owned by user
	pool        string // pool address latch-api hands out
}

func (r *runner) loggedIn() (*apiClient, error) {
	if r.user == nil {
		tok, err := emailLogin(r.anon, r.cfg.email, r.stdin)
		if err != nil {
			return nil, fmt.Errorf("log in %s: %w", r.cfg.email, err)
		}
		r.user = newAPIClient(r.cfg.apiURL, tok)
	}
	return r.user, nil
}

func (r *runner) depositor() (*keypair.Full, error) {
	if r.depositorKP != nil {
		return r.depositorKP, nil
	}
	var kp *keypair.Full
	if r.cfg.depositorSeed != "" {
		parsed, err := keypair.ParseFull(r.cfg.depositorSeed)
		if err != nil {
			return nil, fmt.Errorf("parse depositor seed: %w", err)
		}
		kp = parsed
	} else {
		kp = keypair.MustRandom()
		step("no depositor seed set, funding a fresh one with friendbot")
		if err := friendbot(kp.Address()); err != nil {
			return nil, err
		}
	}
	bal, err := r.chain.xlmBalance(kp.Address())
	if err != nil {
		return nil, fmt.Errorf("depositor %s: %w (fund it: https://friendbot.stellar.org/?addr=%s)", kp.Address(), err, kp.Address())
	}
	step("depositor %s holds %s XLM", kp.Address(), bal)
	r.depositorKP = kp
	return kp, nil
}

// smartAccount deploys a fresh smart account and registers it to the logged-in
// user, once per run.
func (r *runner) smartAccount() (string, error) {
	if r.account != "" {
		return r.account, nil
	}
	user, err := r.loggedIn()
	if err != nil {
		return "", err
	}
	signer := keypair.MustRandom()
	step("deploying a new smart account for signer %s", signer.Address())
	addr, err := deploySmartAccount(r.anon, signer)
	if err != nil {
		return "", err
	}
	step("smart account %s", contractLink(addr))
	if _, err := user.must(http.MethodPost, "/v1/accounts/register", map[string]string{"smart_account_address": addr}, nil); err != nil {
		return "", err
	}
	step("registered to %s", r.cfg.email)
	r.account = addr
	return addr, nil
}

// poolAddress returns the pool latch-api routes deposits to, opening a
// throwaway intent to learn it if no scenario has yet.
func (r *runner) poolAddress() (string, error) {
	if r.pool != "" {
		return r.pool, nil
	}
	acct, err := r.smartAccount()
	if err != nil {
		return "", err
	}
	user, _ := r.loggedIn()
	in, err := createIntent(user, intentRequest{SmartAccountAddress: acct, ExternalID: r.externalID("pool")})
	if err != nil {
		return "", err
	}
	r.pool = in.PoolAddress
	return r.pool, nil
}

func (r *runner) externalID(scenario string) string {
	return "e2e-" + r.runID + "-" + scenario
}

func step(format string, args ...any) {
	fmt.Printf("  · "+format+"\n", args...)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e_live: "+format+"\n", args...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

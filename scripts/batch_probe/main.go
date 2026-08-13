// Command batch_probe answers the two questions the concurrency work is
// blocked on, both against live testnet:
//
//	batch  — how many SAC transfers fit in one Soroban transaction before the
//	         network's per-transaction resource limits reject it. This decides
//	         whether batching is worth building: batching multiplies throughput
//	         inside a ledger slot, whereas extra pool accounts buy more slots at
//	         the cost of more signing keys to secure.
//
//	ledger — whether one account really can only land a single Soroban
//	         transaction per ledger. The whole "shard across pools" conclusion
//	         rests on that being true, and so far it is inferred from timing and
//	         TRY_AGAIN_LATER counts rather than confirmed directly.
//
// Both probes only simulate unless -send is passed, so the default run costs
// nothing and moves no funds.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func main() {
	var (
		mode       = flag.String("mode", "batch", "batch | ledger")
		poolSeed   = flag.String("pool", os.Getenv("POOL_PRIVATE_KEY_1"), "pool secret key")
		destsRaw   = flag.String("dests", "", "comma-separated destination C-addresses")
		maxOps     = flag.Int("max", 40, "highest operation count to probe")
		amount     = flag.String("amount", "0.1", "amount per transfer")
		horizonURL = flag.String("horizon", "https://horizon-testnet.stellar.org", "Horizon URL")
		rpcURL     = flag.String("rpc", "https://soroban-testnet.stellar.org", "Soroban RPC URL")
		send       = flag.Bool("send", false, "actually submit (ledger mode needs this to be meaningful)")
	)
	flag.Parse()

	kp, err := keypair.ParseFull(*poolSeed)
	if err != nil {
		fatal("parse pool secret: %v", err)
	}
	var dests []string
	for _, d := range strings.Split(*destsRaw, ",") {
		if d = strings.TrimSpace(d); d != "" {
			dests = append(dests, d)
		}
	}
	if len(dests) == 0 {
		fatal("-dests is required")
	}

	p := &prober{
		hz:         &horizonclient.Client{HorizonURL: *horizonURL},
		rpc:        rpcclient.NewClient(*rpcURL, nil),
		kp:         kp,
		dests:      dests,
		amount:     *amount,
		passphrase: network.TestNetworkPassphrase,
	}

	ctx := context.Background()
	switch *mode {
	case "batch":
		p.probeBatch(ctx, *maxOps)
	case "ledger":
		p.probeLedger(ctx, *send)
	default:
		fatal("unknown -mode %q", *mode)
	}
}

type prober struct {
	hz         *horizonclient.Client
	rpc        *rpcclient.Client
	kp         *keypair.Full
	dests      []string
	amount     string
	passphrase string
}

// probeBatch walks the operation count upward and reports the largest
// transaction the network will still simulate cleanly.
func (p *prober) probeBatch(ctx context.Context, maxOps int) {
	fmt.Printf("=== batch probe: SAC transfers per Soroban transaction ===\n")
	fmt.Printf("pool %s\n\n", p.kp.Address())
	fmt.Printf("%-6s %-10s %-14s %s\n", "ops", "result", "resource fee", "detail")

	best := 0
	for n := 1; n <= maxOps; n++ {
		fee, err := p.simulate(ctx, n)
		if err != nil {
			fmt.Printf("%-6d %-10s %-14s %s\n", n, "REJECTED", "-", truncate(err.Error(), 90))
			break
		}
		fmt.Printf("%-6d %-10s %-14d %s\n", n, "ok", fee, "")
		best = n

		// Step coarsely once the small sizes are proven, to keep the probe quick.
		if n >= 10 {
			n += 4
		}
	}

	fmt.Printf("\nlargest transaction that simulated cleanly: %d transfers\n", best)
	switch {
	case best >= 5:
		fmt.Printf("=> batching is worth building. One pool could move ~%d deposits per\n", best)
		fmt.Printf("   ledger slot instead of 1, i.e. roughly %d/min instead of ~12/min.\n", best*12)
	case best > 1:
		fmt.Printf("=> batching helps only marginally at %d per transaction; adding pool\n", best)
		fmt.Printf("   accounts is the better lever.\n")
	default:
		fmt.Printf("=> batching is not viable; scale with pool accounts.\n")
	}
}

// probeLedger sends two consecutively-sequenced Soroban transactions from one
// account as fast as possible and reports which ledger each landed in. If the
// one-per-account-per-ledger rule holds they land in different ledgers, or the
// second is refused outright.
func (p *prober) probeLedger(ctx context.Context, send bool) {
	fmt.Printf("=== ledger probe: two Soroban transactions, one account, one window ===\n")
	fmt.Printf("pool %s\n\n", p.kp.Address())
	if !send {
		fmt.Printf("dry run: pass -send to actually submit (this one needs real submission)\n")
		return
	}

	acct, err := p.hz.AccountDetail(horizonclient.AccountRequest{AccountID: p.kp.Address()})
	if err != nil {
		fatal("load pool account: %v", err)
	}
	base, err := acct.GetSequenceNumber()
	if err != nil {
		fatal("sequence: %v", err)
	}

	type outcome struct {
		hash   string
		ledger uint32
		err    error
		at     time.Time
	}
	results := make([]outcome, 2)

	// Built and sent back to back on purpose: the question is what happens when
	// both are in flight inside the same ledger close.
	for i := range 2 {
		hash, err := p.buildSignSend(ctx, base+int64(i)+1, 1)
		results[i] = outcome{hash: hash, err: err, at: time.Now()}
	}

	for i := range results {
		if results[i].err != nil {
			fmt.Printf("tx %d: SEND FAILED — %v\n", i+1, results[i].err)
			continue
		}
		got, err := p.rpc.PollTransaction(ctx, results[i].hash)
		if err != nil {
			fmt.Printf("tx %d: %s poll failed — %v\n", i+1, results[i].hash[:12], err)
			continue
		}
		results[i].ledger = got.Ledger
		fmt.Printf("tx %d: %s status=%s ledger=%d\n", i+1, results[i].hash[:12], got.Status, got.Ledger)
	}

	fmt.Printf("\n")
	switch {
	case results[0].ledger != 0 && results[1].ledger != 0 && results[0].ledger == results[1].ledger:
		fmt.Printf("=> BOTH landed in ledger %d. One account CAN carry multiple Soroban\n", results[0].ledger)
		fmt.Printf("   transactions per ledger, so sharding across pools was the wrong\n")
		fmt.Printf("   conclusion and in-process sequencing should scale on its own.\n")
	case results[0].ledger != 0 && results[1].ledger != 0:
		fmt.Printf("=> Landed in DIFFERENT ledgers (%d and %d), a %d-ledger gap.\n",
			results[0].ledger, results[1].ledger, results[1].ledger-results[0].ledger)
		fmt.Printf("   Confirms one Soroban transaction per account per ledger, which is\n")
		fmt.Printf("   why throughput tracks pool count rather than concurrency.\n")
	case results[0].ledger != 0 && results[1].err != nil:
		// The common outcome, and a positive result rather than a failed probe.
		// TRY_AGAIN_LATER means the queue refused a second transaction from this
		// account; txBAD_SEQ (result code -5) means the sequence had not advanced
		// yet because the first was still unapplied. Either way the account
		// cannot have two Soroban transactions in flight.
		fmt.Printf("=> The second transaction was REFUSED while the first was in flight\n")
		fmt.Printf("   (%v).\n", results[1].err)
		fmt.Printf("   One Soroban transaction per account at a time, so one per ledger.\n")
		fmt.Printf("   Throughput scales with pool accounts, not with concurrency, and\n")
		fmt.Printf("   in-process sequencing cannot help: the sequence only advances once\n")
		fmt.Printf("   the previous transaction is applied on-chain.\n")
	default:
		fmt.Printf("=> Inconclusive: neither transaction confirmed.\n")
	}
}

// simulate builds an n-operation transfer transaction and asks the network to
// simulate it, returning the resource fee it quotes.
func (p *prober) simulate(ctx context.Context, n int) (int64, error) {
	tx, err := p.build(n, 0)
	if err != nil {
		return 0, err
	}
	env := tx.ToXDR()
	raw, err := env.MarshalBinary()
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}
	resp, err := p.rpc.SimulateTransaction(ctx, rpcprotocol.SimulateTransactionRequest{
		Transaction: base64.StdEncoding.EncodeToString(raw),
	})
	if err != nil {
		return 0, fmt.Errorf("simulate: %w", err)
	}
	if resp.Error != "" {
		return 0, fmt.Errorf("%s", resp.Error)
	}
	return resp.MinResourceFee, nil
}

// build assembles a transaction carrying n SAC transfers, cycling through the
// destination list. seq of 0 means "read the account", otherwise the caller
// pins the sequence.
func (p *prober) build(n int, seq int64) (*txnbuild.Transaction, error) {
	var source txnbuild.Account
	if seq == 0 {
		acct, err := p.hz.AccountDetail(horizonclient.AccountRequest{AccountID: p.kp.Address()})
		if err != nil {
			return nil, fmt.Errorf("load account: %w", err)
		}
		source = &acct
	} else {
		source = &txnbuild.SimpleAccount{AccountID: p.kp.Address(), Sequence: seq}
	}

	ops := make([]txnbuild.Operation, 0, n)
	for i := range n {
		op, err := txnbuild.NewPaymentToContract(txnbuild.PaymentToContractParams{
			NetworkPassphrase: p.passphrase,
			Destination:       p.dests[i%len(p.dests)],
			Amount:            p.amount,
			Asset:             txnbuild.NativeAsset{},
			SourceAccount:     p.kp.Address(),
		})
		if err != nil {
			return nil, fmt.Errorf("build op %d: %w", i, err)
		}
		ops = append(ops, &op)
	}

	return txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        source,
		IncrementSequenceNum: seq == 0,
		Operations:           ops,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
}

// buildSignSend simulates, applies the footprint, signs and submits, returning
// the transaction hash once the network accepts it.
func (p *prober) buildSignSend(ctx context.Context, seq int64, ops int) (string, error) {
	tx, err := p.build(ops, seq)
	if err != nil {
		return "", err
	}
	env := tx.ToXDR()
	raw, err := env.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	sim, err := p.rpc.SimulateTransaction(ctx, rpcprotocol.SimulateTransactionRequest{
		Transaction: base64.StdEncoding.EncodeToString(raw),
	})
	if err != nil {
		return "", fmt.Errorf("simulate: %w", err)
	}
	if sim.Error != "" {
		return "", fmt.Errorf("simulate: %s", sim.Error)
	}

	dataBytes, err := base64.StdEncoding.DecodeString(sim.TransactionDataXDR)
	if err != nil {
		return "", fmt.Errorf("decode soroban data: %w", err)
	}
	var sorobanData xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshal(dataBytes, &sorobanData); err != nil {
		return "", fmt.Errorf("unmarshal soroban data: %w", err)
	}

	env.V1.Tx.Ext = xdr.TransactionExt{V: 1, SorobanData: &sorobanData}
	env.V1.Tx.Fee = xdr.Uint32(uint32(txnbuild.MinBaseFee) + uint32(sim.MinResourceFee) + 10_000)

	updated, err := env.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("marshal updated: %w", err)
	}
	generic, err := txnbuild.TransactionFromXDR(base64.StdEncoding.EncodeToString(updated))
	if err != nil {
		return "", fmt.Errorf("reparse: %w", err)
	}
	signable, ok := generic.Transaction()
	if !ok {
		return "", fmt.Errorf("not a simple transaction")
	}
	signed, err := signable.Sign(p.passphrase, p.kp)
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	signedEnv := signed.ToXDR()
	signedBytes, err := signedEnv.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("marshal signed: %w", err)
	}

	resp, err := p.rpc.SendTransaction(ctx, rpcprotocol.SendTransactionRequest{
		Transaction: base64.StdEncoding.EncodeToString(signedBytes),
	})
	if err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	if resp.Status != "PENDING" && resp.Status != "DUPLICATE" {
		return "", fmt.Errorf("send status %s: %s", resp.Status, resp.ErrorResultXDR)
	}
	return resp.Hash, nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

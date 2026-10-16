package sponsor

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/signer"
)

type forwardFixture struct {
	*fixture
	executor  *keypair.Full
	forwarder string
	xlm, usdc string
	walletSc  xdr.ScAddress
	target    xdr.ScAddress
}

func scAddrVal(a xdr.ScAddress) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func symVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func u32Val(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

// forwardArgs builds forward()'s nine arguments.
func (ff *forwardFixture) forwardArgs(t *testing.T, token string, maxFee int64, user xdr.ScAddress) []xdr.ScVal {
	t.Helper()
	_, tokenSc := contractAddrFromString(t, token)
	empty := xdr.ScVec{}
	ep := &empty
	return []xdr.ScVal{
		scAddrVal(tokenSc), i128(0), i128(maxFee), u32Val(6000),
		scAddrVal(ff.target), symVal("transfer"), {Type: xdr.ScValTypeScvVec, Vec: &ep},
		scAddrVal(user), scAddrVal(accountScAddress(t, keypair.MustRandom().Address())),
	}
}

func contractAddrFromString(t *testing.T, c string) (string, xdr.ScAddress) {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, c)
	if err != nil {
		t.Fatal(err)
	}
	var id xdr.ContractId
	copy(id[:], raw)
	return c, xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
}

func newForwardFixture(t *testing.T) *forwardFixture {
	t.Helper()
	f := newFixture(t)
	ff := &forwardFixture{fixture: f, executor: keypair.MustRandom()}
	ff.forwarder, _ = contractAddr(t, 0x40)
	ff.xlm, _ = contractAddr(t, 0x41)
	ff.usdc, _ = contractAddr(t, 0x42)
	_, ff.walletSc = contractAddr(t, 1)
	_, ff.target = contractAddr(t, 0x43)
	f.sub.Forward = &Forward{
		ForwarderID: ff.forwarder,
		Executor:    signer.FromKeypair(ff.executor),
		Tokens: map[string]FeeToken{
			ff.xlm:  {Contract: ff.xlm, Symbol: "XLM", Native: true},
			ff.usdc: {Contract: ff.usdc, Symbol: "USDC"},
		},
		Prices:      FixedPrice(0.2),
		MarginBps:   2500,
		AuthLedgers: 60,
	}
	f.sub.RelayerAccounts[ff.executor.Address()] = true
	return ff
}

// request builds a forward-mode request whose user entry is signed (the
// signature itself isn't checked by the fake) and records an executor entry.
func (ff *forwardFixture) request(t *testing.T, token string, maxFee int64) Request {
	t.Helper()
	_, fwdSc := contractAddrFromString(t, ff.forwarder)
	invoke := xdr.InvokeContractArgs{ContractAddress: fwdSc, FunctionName: "forward", Args: ff.forwardArgs(t, token, maxFee, ff.walletSc)}
	userEntry := addressAuth(ff.walletSc, invokeArgs(fwdSc, "forward"))

	execEntry := addressAuth(accountScAddress(t, ff.executor.Address()), invoke)
	b64, err := xdr.MarshalBase64(execEntry)
	if err != nil {
		t.Fatal(err)
	}
	ff.rpc.recordAuth = []string{b64}

	wallet, _ := contractAddr(t, 1)
	return Request{RequestID: "fwd_000001", Wallet: wallet, Mode: ModeForward,
		Transaction: envelopeB64(t, invokeOp(invoke, userEntry))}
}

func TestForward_PricesSignsAndReserves(t *testing.T) {
	ff := newForwardFixture(t)
	ff.req = ff.request(t, ff.xlm, 1_000_000)

	p, existing, err := ff.sub.Prepare(context.Background(), ff.req)
	if err != nil || existing {
		t.Fatalf("prepare: existing=%v err=%v", existing, err)
	}

	// Cost = 2*p90 inclusion (300) + resource fee (50,000) = 50,600 stroops,
	// +25% margin = 63,250 XLM units.
	const wantFee = 63_250
	args := p.call.HostFunction.MustInvokeContract().Args
	if got, _ := scI128(args[argFeeAmount]); got != wantFee {
		t.Fatalf("fee_amount = %d, want %d", got, wantFee)
	}
	if relayer, _ := scAddr(args[argRelayer]); relayer != ff.executor.Address() {
		t.Fatalf("relayer arg = %s, want the executor", relayer)
	}

	// The user's entry is kept as signed; the executor's is added and signed.
	if len(p.call.Auth) != 2 {
		t.Fatalf("auth entries = %d, want user + executor", len(p.call.Auth))
	}
	exec := p.call.Auth[1]
	if addr, _ := authAddress(exec.Credentials); addr != ff.executor.Address() {
		t.Fatalf("second entry is for %s", addr)
	}
	if got := uint32(exec.Credentials.Address.SignatureExpirationLedger); got != 5000+60 {
		t.Fatalf("executor signature expires at %d", got)
	}
	if got, _ := scI128(exec.RootInvocation.Function.ContractFn.Args[argFeeAmount]); got != wantFee {
		t.Fatalf("executor signed fee_amount %d, want %d", got, wantFee)
	}
	verifyExecutorSignature(t, ff.executor, exec)

	// Record mode first (no auth sent), then the enforce-mode pricing probe,
	// then Prepare's enforce-mode run the transaction is built from.
	if len(ff.rpc.simModes) != 3 || ff.rpc.simModes[0] != "record" || ff.rpc.simModes[1] != "enforce" || ff.rpc.simModes[2] != "enforce" {
		t.Fatalf("simulation modes = %v", ff.rpc.simModes)
	}
	var recorded xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(ff.rpc.simTxs[0], &recorded); err != nil {
		t.Fatal(err)
	}
	if n := len(recorded.V1.Tx.Operations[0].Body.InvokeHostFunctionOp.Auth); n != 0 {
		t.Fatalf("record-mode simulation sent %d auth entries", n)
	}

	res := ff.records.reserved[0]
	if res.UserFee == nil || res.UserFee.Amount != wantFee || res.UserFee.Symbol != "XLM" {
		t.Fatalf("reservation user fee = %+v", res.UserFee)
	}
	if res.Function != "transfer" {
		t.Fatalf("recorded function = %s, want the target's", res.Function)
	}

	if rec := ff.sub.Run(context.Background(), p); rec.Status != StatusSuccess {
		t.Fatalf("run: %+v", rec)
	}
}

// TestForward_PricesFromEnforceMode: record mode overestimates the resource
// fee (it did by ~40% on mainnet); the user is charged from the enforce-mode
// simulation the transaction is built from, not the record-mode estimate.
func TestForward_PricesFromEnforceMode(t *testing.T) {
	ff := newForwardFixture(t)
	ff.rpc.recordResource = 70_000 // enforce mode stays at 50,000
	ff.req = ff.request(t, ff.xlm, 1_000_000)

	p, _, err := ff.sub.Prepare(context.Background(), ff.req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// Probe: signed at the record-mode estimate, (2×300 + 70,000) +25% = 88,250.
	var probe xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(ff.rpc.simTxs[1], &probe); err != nil {
		t.Fatal(err)
	}
	probeArgs := probe.V1.Tx.Operations[0].Body.InvokeHostFunctionOp.HostFunction.MustInvokeContract().Args
	if got, _ := scI128(probeArgs[argFeeAmount]); got != 88_250 {
		t.Fatalf("probe fee_amount = %d, want the record-mode estimate 88,250", got)
	}

	// Charged: (2×300 + 50,000) +25% = 63,250, and the executor signed that.
	const wantFee = 63_250
	args := p.call.HostFunction.MustInvokeContract().Args
	if got, _ := scI128(args[argFeeAmount]); got != wantFee {
		t.Fatalf("fee_amount = %d, want %d", got, wantFee)
	}
	exec := p.call.Auth[len(p.call.Auth)-1]
	if got, _ := scI128(exec.RootInvocation.Function.ContractFn.Args[argFeeAmount]); got != wantFee {
		t.Fatalf("executor signed fee_amount %d, want %d", got, wantFee)
	}
	verifyExecutorSignature(t, ff.executor, exec)
	if res := ff.records.reserved[0]; res.UserFee == nil || res.UserFee.Amount != wantFee {
		t.Fatalf("reservation user fee = %+v", res.UserFee)
	}
}

// TestForward_EnforceModeOverMaxIsRefused: a fee the record-mode estimate
// allowed but the enforce-mode price exceeds is refused, never sent.
func TestForward_EnforceModeOverMaxIsRefused(t *testing.T) {
	ff := newForwardFixture(t)
	ff.rpc.recordResource = 10_000
	ff.rpc.resource = 900_000 // (2×300 + 900,000) +25% > 1,000,000
	ff.req = ff.request(t, ff.xlm, 1_000_000)

	if _, _, err := ff.sub.Prepare(context.Background(), ff.req); !errors.Is(err, ErrFeeTooLow) {
		t.Fatalf("err = %v, want ErrFeeTooLow", err)
	}
	if len(ff.records.reserved) != 0 {
		t.Fatal("reserved despite the fee exceeding the user's maximum")
	}
}

func verifyExecutorSignature(t *testing.T, kp *keypair.Full, e xdr.SorobanAuthorizationEntry) {
	t.Helper()
	c := e.Credentials.Address
	pre := xdr.HashIdPreimage{Type: xdr.EnvelopeTypeEnvelopeTypeSorobanAuthorization,
		SorobanAuthorization: &xdr.HashIdPreimageSorobanAuthorization{
			NetworkId: xdr.Hash(sha256.Sum256([]byte(network.TestNetworkPassphrase))),
			Nonce:     c.Nonce, SignatureExpirationLedger: c.SignatureExpirationLedger, Invocation: e.RootInvocation,
		}}
	raw, err := pre.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	vec := **c.Signature.Vec
	m := **vec[0].Map
	if string(*m[0].Key.Sym) != "public_key" || string(*m[1].Key.Sym) != "signature" {
		t.Fatalf("signature map keys = %v, %v", *m[0].Key.Sym, *m[1].Key.Sym)
	}
	if err := kp.Verify(hash[:], *m[1].Val.Bytes); err != nil {
		t.Fatalf("executor signature does not verify: %v", err)
	}
}

func TestForward_USDCIsPriced(t *testing.T) {
	ff := newForwardFixture(t)
	ff.req = ff.request(t, ff.usdc, 1_000_000)
	if _, _, err := ff.sub.Prepare(context.Background(), ff.req); err != nil {
		t.Fatal(err)
	}
	// 50,600 stroops × 1.25 × $0.20 = 12,650 USDC units.
	if got := ff.records.reserved[0].UserFee; got.Amount != 12_650 || got.Symbol != "USDC" {
		t.Fatalf("user fee = %+v", got)
	}
}

func TestForward_Refusals(t *testing.T) {
	t.Run("max fee below cost", func(t *testing.T) {
		ff := newForwardFixture(t)
		ff.req = ff.request(t, ff.xlm, 10)
		if _, _, err := ff.sub.Prepare(context.Background(), ff.req); !errors.Is(err, ErrFeeTooLow) {
			t.Fatalf("err = %v", err)
		}
		if len(ff.records.reserved) != 0 {
			t.Fatal("reserved despite a fee below cost")
		}
	})
	t.Run("no price for USDC", func(t *testing.T) {
		ff := newForwardFixture(t)
		ff.sub.Forward.Prices = FixedPrice(0)
		ff.req = ff.request(t, ff.usdc, 1_000_000)
		if _, _, err := ff.sub.Prepare(context.Background(), ff.req); !errors.Is(err, ErrPriceUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("forward mode not configured", func(t *testing.T) {
		ff := newForwardFixture(t)
		ff.req = ff.request(t, ff.xlm, 1_000_000)
		ff.sub.Forward = nil
		if _, _, err := ff.sub.Prepare(context.Background(), ff.req); !errors.Is(err, ErrForwardNotBuilt) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestParseForward(t *testing.T) {
	ff := newForwardFixture(t)
	f := ff.sub.Forward
	wallet, _ := contractAddr(t, 1)
	_, fwdSc := contractAddrFromString(t, ff.forwarder)
	mk := func(contract xdr.ScAddress, fn string, args []xdr.ScVal) Call {
		inv := xdr.InvokeContractArgs{ContractAddress: contract, FunctionName: xdr.ScSymbol(fn), Args: args}
		c, _ := scAddressString(contract)
		return Call{Contract: c, Function: fn, HostFunction: xdr.HostFunction{Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract, InvokeContract: &inv}}
	}
	good := ff.forwardArgs(t, ff.xlm, 1000, ff.walletSc)
	if _, err := f.parseForward(mk(fwdSc, "forward", good), wallet); err != nil {
		t.Fatalf("valid: %v", err)
	}

	mutate := func(i int, v xdr.ScVal) []xdr.ScVal {
		a := append([]xdr.ScVal(nil), good...)
		a[i] = v
		return a
	}
	other, otherSc := contractAddr(t, 0x55)
	_ = other
	_, unknownToken := contractAddr(t, 0x56)
	cases := map[string]Call{
		"wrong contract":      mk(otherSc, "forward", good),
		"wrong function":      mk(fwdSc, "forward2", good),
		"too few args":        mk(fwdSc, "forward", good[:8]),
		"token not accepted":  mk(fwdSc, "forward", mutate(argFeeToken, scAddrVal(unknownToken))),
		"zero max fee":        mk(fwdSc, "forward", mutate(argMaxFee, i128(0))),
		"user is not wallet":  mk(fwdSc, "forward", mutate(argUser, scAddrVal(otherSc))),
		"target is forwarder": mk(fwdSc, "forward", mutate(argTarget, scAddrVal(fwdSc))),
		"expiration not u32":  mk(fwdSc, "forward", mutate(argExpiration, i128(1))),
	}
	for name, call := range cases {
		if _, err := f.parseForward(call, wallet); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestQuote(t *testing.T) {
	ff := newForwardFixture(t)
	q, err := ff.sub.Quote(context.Background(), ff.xlm, 50_000)
	if err != nil {
		t.Fatal(err)
	}
	// (2 × 20,000 max inclusion + 50,000) × 1.25 = 112,500.
	if q.MaxFeeAmount != 112_500 || q.Relayer != ff.executor.Address() || q.FeeForwarder != ff.forwarder {
		t.Fatalf("quote = %+v", q)
	}
	if _, err := ff.sub.Quote(context.Background(), keypair.MustRandom().Address(), 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestFeeInTokenRoundsUp(t *testing.T) {
	f := &Forward{MarginBps: 0, Prices: FixedPrice(0.3)}
	got, err := f.FeeInToken(context.Background(), FeeToken{Symbol: "USDC"}, 7)
	if err != nil || got != 3 { // 7 × 0.3 = 2.1 → 3
		t.Fatalf("got %d err %v", got, err)
	}
}

// seqPrice returns its prices in turn, then repeats the last.
type seqPrice struct {
	prices []float64
	errs   []error
	i      int
}

func (p *seqPrice) XLMUSD(context.Context) (float64, error) {
	i := min(p.i, len(p.prices)-1)
	p.i++
	var err error
	if i < len(p.errs) {
		err = p.errs[i]
	}
	return p.prices[i], err
}

func TestFeeInToken_PriceFallback(t *testing.T) {
	usdc := FeeToken{Symbol: "USDC"}
	var reasons []string
	newF := func(src PriceSource) *Forward {
		reasons = nil
		return &Forward{MinXLMUSD: 0.01, MaxXLMUSD: 10, PriceMaxStale: 30 * time.Minute, Prices: src,
			OnPriceFallback: func(r string) { reasons = append(reasons, r) }}
	}

	// No good price yet: an implausible one is refused, never used.
	for _, p := range []float64{0.0000001, 0.009, 11, 1e9} {
		f := newF(FixedPrice(p))
		if _, err := f.FeeInToken(context.Background(), usdc, 1000); !errors.Is(err, ErrPriceUnavailable) {
			t.Errorf("price %v with no last good: err = %v", p, err)
		}
		if len(reasons) != 1 || reasons[0] != "refused" {
			t.Errorf("price %v: reasons = %v", p, reasons)
		}
	}

	// After a good price, a bad one (out of range, then unavailable) falls back to it.
	src := &seqPrice{prices: []float64{0.2, 1e9, 0}, errs: []error{nil, nil, errors.New("feed down")}}
	f := newF(src)
	for i, want := range []int64{200, 200, 200} {
		got, err := f.FeeInToken(context.Background(), usdc, 1000)
		if err != nil || got != want {
			t.Fatalf("call %d: %d %v", i, got, err)
		}
	}
	if len(reasons) != 2 || reasons[0] != "out_of_range" || reasons[1] != "unavailable" {
		t.Fatalf("reasons = %v", reasons)
	}

	// Past PriceMaxStale the last good price is no longer used.
	f.lastGoodAt = time.Now().Add(-31 * time.Minute)
	if _, err := f.FeeInToken(context.Background(), usdc, 1000); !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("stale last good: err = %v", err)
	}

	// XLM needs no price, so a broken feed doesn't stop XLM fees.
	if got, err := newF(FixedPrice(1e9)).FeeInToken(context.Background(), FeeToken{Native: true}, 1000); err != nil || got != 1000 {
		t.Fatalf("native with a bad feed: %d %v", got, err)
	}
}

func TestFeeInToken_NoOverflow(t *testing.T) {
	f := &Forward{MarginBps: 50_000, Prices: FixedPrice(10), MaxXLMUSD: 10}
	// stroops × (10,000 + margin) would pass int64 in plain arithmetic.
	big := int64(1) << 62 // × 6 = 2.8e19, past int64
	if _, err := f.FeeInToken(context.Background(), FeeToken{Native: true}, big); err == nil {
		t.Fatal("a result past int64 must be an error, not a wrapped number")
	}
	got, err := f.FeeInToken(context.Background(), FeeToken{Native: true}, MaxResourceFeeStroops)
	if err != nil || got != 6*MaxResourceFeeStroops {
		t.Fatalf("largest accepted cost: %d %v", got, err)
	}
	if _, err := f.FeeInToken(context.Background(), FeeToken{Native: true}, -1); err == nil {
		t.Fatal("negative cost must be refused")
	}
}

func TestQuote_RejectsOutOfRangeResourceFee(t *testing.T) {
	ff := newForwardFixture(t)
	for _, fee := range []int64{-1, MaxResourceFeeStroops + 1, 1 << 62} {
		if _, err := ff.sub.Quote(context.Background(), ff.xlm, fee); !errors.Is(err, ErrInvalid) {
			t.Errorf("resource fee %d: err = %v", fee, err)
		}
	}
}

func TestForward_ImplausibleResourceFeeIsRefused(t *testing.T) {
	ff := newForwardFixture(t)
	ff.req = ff.request(t, ff.xlm, 1_000_000)
	ff.rpc.resource = MaxResourceFeeStroops + 1
	if _, _, err := ff.sub.Prepare(context.Background(), ff.req); !errors.Is(err, ErrSimulation) {
		t.Fatalf("err = %v", err)
	}
	if len(ff.records.reserved) != 0 {
		t.Fatal("reserved despite an implausible resource fee")
	}
}

func TestStellarExpertPrice_BoundedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A body far past the limit, with the price at the end.
		_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", 2*maxPriceBody) + `","price":0.2}`))
	}))
	defer srv.Close()
	p := &StellarExpertPrice{URL: srv.URL, TTL: time.Minute}
	if _, err := p.XLMUSD(context.Background()); err == nil {
		t.Fatal("an over-long body must be refused, not read")
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"asset":"XLM","price":0.2}`))
	}))
	defer ok.Close()
	p = &StellarExpertPrice{URL: ok.URL, TTL: time.Minute}
	if got, err := p.XLMUSD(context.Background()); err != nil || got != 0.2 {
		t.Fatalf("normal body: %v %v", got, err)
	}
}

package chain

import (
	"context"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const forwarder = "CB6KFDFN7CXIOSBOEXABOX6KPRK4X6VMPSDWCGYP564JXGN2LIH2QPWI"

// fakeRPC serves ledger entries from a map and answers simulations with a
// canned return value, recording what it was asked.
type fakeRPC struct {
	accounts   map[string]xdr.AccountEntry      // by address
	data       map[string]xdr.ContractDataEntry // by base64 ledger key
	calls      int
	simReturn  *xdr.ScVal
	simError   string
	simRequest string
}

func (f *fakeRPC) GetNetwork(context.Context) (protocol.GetNetworkResponse, error) {
	return protocol.GetNetworkResponse{Passphrase: network.TestNetworkPassphrase}, nil
}

func (f *fakeRPC) GetLedgerEntries(_ context.Context, req protocol.GetLedgerEntriesRequest) (protocol.GetLedgerEntriesResponse, error) {
	f.calls++
	var resp protocol.GetLedgerEntriesResponse
	for _, key := range req.Keys {
		var lk xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(key, &lk); err != nil {
			return resp, err
		}
		if lk.Type == xdr.LedgerEntryTypeContractData {
			cd, ok := f.data[key]
			if !ok {
				continue
			}
			data, _ := xdr.NewLedgerEntryData(xdr.LedgerEntryTypeContractData, cd)
			b64, _ := xdr.MarshalBase64(data)
			resp.Entries = append(resp.Entries, protocol.LedgerEntryResult{KeyXDR: key, DataXDR: b64})
			continue
		}
		addr := lk.Account.AccountId.Address()
		acc, ok := f.accounts[addr]
		if !ok {
			continue
		}
		data, _ := xdr.NewLedgerEntryData(xdr.LedgerEntryTypeAccount, acc)
		b64, _ := xdr.MarshalBase64(data)
		resp.Entries = append(resp.Entries, protocol.LedgerEntryResult{KeyXDR: key, DataXDR: b64})
	}
	return resp, nil
}

func (f *fakeRPC) SimulateTransaction(_ context.Context, req protocol.SimulateTransactionRequest) (protocol.SimulateTransactionResponse, error) {
	f.simRequest = req.Transaction
	if f.simError != "" {
		return protocol.SimulateTransactionResponse{Error: f.simError}, nil
	}
	b64, _ := xdr.MarshalBase64(*f.simReturn)
	return protocol.SimulateTransactionResponse{
		Results: []protocol.SimulateHostFunctionResult{{ReturnValueXDR: &b64}},
	}, nil
}

func accountEntry(addr string, balance, seq int64) xdr.AccountEntry {
	return xdr.AccountEntry{
		AccountId: xdr.MustAddress(addr),
		Balance:   xdr.Int64(balance),
		SeqNum:    xdr.SequenceNumber(seq),
	}
}

func TestAccountsBatchesAndReportsMissing(t *testing.T) {
	f := &fakeRPC{accounts: map[string]xdr.AccountEntry{}}
	var addrs []string
	for i := 0; i < 450; i++ {
		a := keypair.MustRandom().Address()
		addrs = append(addrs, a)
		if i%2 == 0 {
			f.accounts[a] = accountEntry(a, int64(i)*10, int64(i)+100)
		}
	}

	got, err := Accounts(context.Background(), f, addrs)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 3 {
		t.Fatalf("450 accounts took %d RPC calls, want 3 (batches of 200)", f.calls)
	}
	if a := got[addrs[4]]; !a.Exists || a.BalanceStroops != 40 || a.Seq != 104 {
		t.Fatalf("existing account decoded wrong: %+v", a)
	}
	if got[addrs[1]].Exists {
		t.Fatal("missing account reported as existing")
	}
}

const (
	xlmSAC  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// sacBalanceKey is the ledger key a SAC stores holder's balance under.
func sacBalanceKey(t *testing.T, token, holder string) (string, xdr.ScAddress, xdr.ScVal) {
	t.Helper()
	tokenAddr, err := ScAddressFromString(token)
	if err != nil {
		t.Fatal(err)
	}
	holderVal, err := ScAddressVal(holder)
	if err != nil {
		t.Fatal(err)
	}
	vec := xdr.ScVec{ScSymbol("Balance"), holderVal}
	vecPtr := &vec
	k := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vecPtr}
	b64, err := xdr.MarshalBase64(xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.LedgerKeyContractData{
		Contract: tokenAddr, Key: k, Durability: xdr.ContractDataDurabilityPersistent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b64, tokenAddr, k
}

func sacBalanceEntry(tokenAddr xdr.ScAddress, key xdr.ScVal, amount uint64) xdr.ContractDataEntry {
	f := false
	tr := true
	m := xdr.ScMap{
		{Key: ScSymbol("amount"), Val: xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(amount)}}},
		{Key: ScSymbol("authorized"), Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &tr}},
		{Key: ScSymbol("clawback"), Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &f}},
	}
	mp := &m
	return xdr.ContractDataEntry{Contract: tokenAddr, Key: key, Durability: xdr.ContractDataDurabilityPersistent,
		Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}}
}

func TestTokenBalances(t *testing.T) {
	f := &fakeRPC{data: map[string]xdr.ContractDataEntry{}}
	key, tokenAddr, k := sacBalanceKey(t, xlmSAC, forwarder)
	f.data[key] = sacBalanceEntry(tokenAddr, k, 885_535)

	got, err := TokenBalances(context.Background(), f, forwarder, []string{xlmSAC, usdcSAC})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("took %d RPC calls, want 1", f.calls)
	}
	if got[xlmSAC] != 885_535 {
		t.Fatalf("XLM balance = %d, want 885,535", got[xlmSAC])
	}
	if v, ok := got[usdcSAC]; !ok || v != 0 {
		t.Fatalf("missing USDC entry = %d (present %v), want 0", v, ok)
	}
}

func TestTokenBalancesRejectsBadHolder(t *testing.T) {
	if _, err := TokenBalances(context.Background(), &fakeRPC{}, "not-an-address", []string{xlmSAC}); err == nil {
		t.Fatal("bad holder accepted")
	}
}

func TestHasRole(t *testing.T) {
	u := xdr.Uint32(1)
	cases := map[string]struct {
		ret  xdr.ScVal
		want bool
	}{
		"granted (Some)":     {xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}, true},
		"not granted (None)": {xdr.ScVal{Type: xdr.ScValTypeScvVoid}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeRPC{simReturn: &tc.ret}
			executor := keypair.MustRandom().Address()
			got, err := HasRole(context.Background(), f, network.TestNetworkPassphrase,
				keypair.MustRandom().Address(), forwarder, executor, "executor")
			if err != nil || got != tc.want {
				t.Fatalf("HasRole = %v, %v; want %v", got, err, tc.want)
			}
			assertHasRoleCall(t, f.simRequest, executor)
		})
	}
}

func TestHasRoleSurfacesSimulationError(t *testing.T) {
	f := &fakeRPC{simError: "HostError: contract not found"}
	if _, err := HasRole(context.Background(), f, network.TestNetworkPassphrase,
		keypair.MustRandom().Address(), forwarder, keypair.MustRandom().Address(), "executor"); err == nil {
		t.Fatal("expected the simulation error to surface")
	}
}

// The simulated transaction must call forwarder.has_role(executor, "executor").
func assertHasRoleCall(t *testing.T, txB64, executor string) {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(txB64)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := parsed.Transaction()
	op := tx.Operations()[0].(*txnbuild.InvokeHostFunction)
	ic := op.HostFunction.InvokeContract
	contract, _ := ic.ContractAddress.String()
	if contract != forwarder || string(ic.FunctionName) != "has_role" || len(ic.Args) != 2 {
		t.Fatalf("wrong call: %s.%s(%d args)", contract, ic.FunctionName, len(ic.Args))
	}
	arg0, _ := ic.Args[0].Address.String()
	if arg0 != executor || string(*ic.Args[1].Sym) != "executor" {
		t.Fatalf("wrong args: %s, %v", arg0, ic.Args[1])
	}
}

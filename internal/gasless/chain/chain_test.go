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
	accounts   map[string]xdr.AccountEntry // by address
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

package startup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"
)

type fakeRPC struct {
	passphrase  string
	existing    map[string]bool
	hasRole     bool
	networkErrs int // fail this many GetNetwork calls first
}

func (f *fakeRPC) GetNetwork(context.Context) (protocol.GetNetworkResponse, error) {
	if f.networkErrs > 0 {
		f.networkErrs--
		return protocol.GetNetworkResponse{}, errors.New("connection refused")
	}
	return protocol.GetNetworkResponse{Passphrase: f.passphrase}, nil
}

func (f *fakeRPC) GetLedgerEntries(_ context.Context, req protocol.GetLedgerEntriesRequest) (protocol.GetLedgerEntriesResponse, error) {
	var resp protocol.GetLedgerEntriesResponse
	for _, key := range req.Keys {
		var lk xdr.LedgerKey
		_ = xdr.SafeUnmarshalBase64(key, &lk)
		addr := lk.Account.AccountId.Address()
		if !f.existing[addr] {
			continue
		}
		data, _ := xdr.NewLedgerEntryData(xdr.LedgerEntryTypeAccount, xdr.AccountEntry{AccountId: xdr.MustAddress(addr), Balance: 1})
		b64, _ := xdr.MarshalBase64(data)
		resp.Entries = append(resp.Entries, protocol.LedgerEntryResult{KeyXDR: key, DataXDR: b64})
	}
	return resp, nil
}

func (f *fakeRPC) SimulateTransaction(context.Context, protocol.SimulateTransactionRequest) (protocol.SimulateTransactionResponse, error) {
	ret := xdr.ScVal{Type: xdr.ScValTypeScvVoid}
	if f.hasRole {
		u := xdr.Uint32(0)
		ret = xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}
	}
	b64, _ := xdr.MarshalBase64(ret)
	return protocol.SimulateTransactionResponse{Results: []protocol.SimulateHostFunctionResult{{ReturnValueXDR: &b64}}}, nil
}

func setup() (*fakeRPC, Check) {
	exec, fund := keypair.MustRandom().Address(), keypair.MustRandom().Address()
	return &fakeRPC{
			passphrase: network.TestNetworkPassphrase,
			existing:   map[string]bool{exec: true, fund: true},
			hasRole:    true,
		}, Check{
			Passphrase:     network.TestNetworkPassphrase,
			FeeForwarderID: "CB6KFDFN7CXIOSBOEXABOX6KPRK4X6VMPSDWCGYP564JXGN2LIH2QPWI",
			Executor:       exec,
			Funder:         fund,
		}
}

func TestPreflightPasses(t *testing.T) {
	f, c := setup()
	if err := Preflight(context.Background(), f, c, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightMisconfigurationsFailFastWithoutRetry(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeRPC, *Check)
		want   string
	}{
		"wrong network":    {func(f *fakeRPC, _ *Check) { f.passphrase = network.PublicNetworkPassphrase }, "NETWORK expects"},
		"missing funder":   {func(f *fakeRPC, c *Check) { delete(f.existing, c.Funder) }, "funder account"},
		"executor no role": {func(f *fakeRPC, _ *Check) { f.hasRole = false }, "grant_role"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, c := setup()
			tc.mutate(f, &c)
			start := time.Now()
			err := Preflight(context.Background(), f, c, 10*time.Second)
			if !errors.Is(err, ErrMisconfigured) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want ErrMisconfigured mentioning %q", err, tc.want)
			}
			if time.Since(start) > time.Second {
				t.Fatal("a misconfiguration was retried instead of failing fast")
			}
		})
	}
}

func TestPreflightRetriesTransientErrors(t *testing.T) {
	f, c := setup()
	f.networkErrs = 1 // RPC briefly unreachable at boot
	if err := Preflight(context.Background(), f, c, 5*time.Second); err != nil {
		t.Fatalf("transient error was not retried: %v", err)
	}
}

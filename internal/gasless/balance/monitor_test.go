package balance

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/gasless/channels"
	"github.com/latch/relayer/internal/gasless/keys"
)

type fakeRPC struct {
	balances map[string]int64 // missing = account doesn't exist
	fail     bool
}

func (f *fakeRPC) GetNetwork(context.Context) (protocol.GetNetworkResponse, error) {
	return protocol.GetNetworkResponse{}, nil
}

func (f *fakeRPC) SimulateTransaction(context.Context, protocol.SimulateTransactionRequest) (protocol.SimulateTransactionResponse, error) {
	return protocol.SimulateTransactionResponse{}, nil
}

func (f *fakeRPC) GetLedgerEntries(_ context.Context, req protocol.GetLedgerEntriesRequest) (protocol.GetLedgerEntriesResponse, error) {
	var resp protocol.GetLedgerEntriesResponse
	if f.fail {
		return resp, errors.New("rpc down")
	}
	for _, key := range req.Keys {
		var lk xdr.LedgerKey
		_ = xdr.SafeUnmarshalBase64(key, &lk)
		addr := lk.Account.AccountId.Address()
		bal, ok := f.balances[addr]
		if !ok {
			continue
		}
		data, _ := xdr.NewLedgerEntryData(xdr.LedgerEntryTypeAccount, xdr.AccountEntry{AccountId: xdr.MustAddress(addr), Balance: xdr.Int64(bal)})
		b64, _ := xdr.MarshalBase64(data)
		resp.Entries = append(resp.Entries, protocol.LedgerEntryResult{KeyXDR: key, DataXDR: b64})
	}
	return resp, nil
}

type fakeStore struct{ recorded map[int]int64 }

func (s *fakeStore) RecordBalance(_ context.Context, index int, bal, _ int64) error {
	s.recorded[index] = bal
	return nil
}

func (s *fakeStore) Stats(context.Context) (channels.Stats, error) {
	return channels.Stats{Active: 1, Disabled: 1}, nil
}

func newMonitor(t *testing.T, rpc *fakeRPC) (*Monitor, *fakeStore, Config) {
	t.Helper()
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	chans, _ := keys.DeriveChannels(seed, 2)
	cfg := Config{
		Executor:          keypair.MustRandom().Address(),
		Funder:            keypair.MustRandom().Address(),
		Channels:          chans,
		FunderMinStroops:  500,
		ChannelMinStroops: 10,
		Interval:          time.Minute,
	}
	store := &fakeStore{recorded: map[int]int64{}}
	return NewMonitor(rpc, store, cfg, prometheus.NewRegistry(), "test"), store, cfg
}

func TestCheckRecordsBalancesAndGatesSponsorship(t *testing.T) {
	rpc := &fakeRPC{balances: map[string]int64{}}
	m, store, cfg := newMonitor(t, rpc)

	if m.SponsorshipOK() {
		t.Fatal("sponsorship must be off before the first balance check")
	}

	rpc.balances[cfg.Funder] = 1000
	rpc.balances[cfg.Executor] = 20
	rpc.balances[cfg.Channels[0].Address()] = 15 // channel 1 not created yet
	m.Check(context.Background())

	if !m.SponsorshipOK() {
		t.Fatal("funder above floor should allow sponsorship")
	}
	if store.recorded[0] != 15 || store.recorded[1] != -1 {
		t.Fatalf("channel balances recorded as %v; want 0→15, 1→-1 (missing)", store.recorded)
	}

	rpc.balances[cfg.Funder] = 499
	m.Check(context.Background())
	if m.SponsorshipOK() {
		t.Fatal("funder below floor must stop sponsorship")
	}
}

// An RPC blip must not flip sponsorship off; the last good snapshot stands.
func TestCheckKeepsLastSnapshotOnRPCFailure(t *testing.T) {
	rpc := &fakeRPC{balances: map[string]int64{}}
	m, _, cfg := newMonitor(t, rpc)
	rpc.balances[cfg.Funder] = 1000
	m.Check(context.Background())

	rpc.fail = true
	m.Check(context.Background())
	if !m.SponsorshipOK() {
		t.Fatal("a failed balance read flipped sponsorship off")
	}
}

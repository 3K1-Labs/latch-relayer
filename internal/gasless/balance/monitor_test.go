package balance

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/gasless/chain"
	"github.com/latch/relayer/internal/gasless/channels"
	"github.com/latch/relayer/internal/gasless/keys"
)

type fakeRPC struct {
	balances map[string]int64 // missing = account doesn't exist
	// fees is FeeForwarder's balance per SAC contract; missing = no entry.
	fees map[string]uint64
	fail bool
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
		if lk.Type == xdr.LedgerEntryTypeContractData {
			token, _ := lk.ContractData.Contract.String()
			units, ok := f.fees[token]
			if !ok {
				continue
			}
			m := xdr.ScMap{{Key: chain.ScSymbol("amount"), Val: xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(units)}}}}
			mp := &m
			data, _ := xdr.NewLedgerEntryData(xdr.LedgerEntryTypeContractData, xdr.ContractDataEntry{
				Contract: lk.ContractData.Contract, Key: lk.ContractData.Key, Durability: lk.ContractData.Durability,
				Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp},
			})
			b64, _ := xdr.MarshalBase64(data)
			resp.Entries = append(resp.Entries, protocol.LedgerEntryResult{KeyXDR: key, DataXDR: b64})
			continue
		}
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

const (
	testForwarder = "CCQXTY4EG56OP7APGGJSY5YITFNSXWB4EKPZ7KKMYEHW7BXIKW44HSTA"
	testXLMSAC    = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	testUSDCSAC   = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// captureLogs routes slog to a buffer for the test's duration.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestCheckAlertsOnceWhenForwarderFeesNeedSweeping(t *testing.T) {
	logs := captureLogs(t)
	rpc := &fakeRPC{balances: map[string]int64{}, fees: map[string]uint64{testXLMSAC: 5_000_000}}
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	chans, _ := keys.DeriveChannels(seed, 1)
	m := NewMonitor(rpc, &fakeStore{recorded: map[int]int64{}}, Config{
		Executor: keypair.MustRandom().Address(), Funder: keypair.MustRandom().Address(),
		Channels: chans, Interval: time.Minute,
		Forwarder: testForwarder, FeeTokens: map[string]string{testXLMSAC: "XLM", testUSDCSAC: "USDC"},
		SweepAlertUnits: 10_000_000,
	}, prometheus.NewRegistry(), "test")
	alerts := func() int { return strings.Count(logs.String(), "FeeForwarder holds collected fees") }

	m.Check(context.Background())
	fees := m.Snapshot().ForwarderFees
	if fees["XLM"] != 5_000_000 || fees["USDC"] != 0 {
		t.Fatalf("forwarder fees = %v, want XLM 5,000,000 and USDC 0", fees)
	}
	if alerts() != 0 {
		t.Fatal("alerted below the threshold")
	}

	rpc.fees[testXLMSAC] = 12_000_000
	m.Check(context.Background())
	m.Check(context.Background())
	if alerts() != 1 {
		t.Fatalf("alerted %d times while over the threshold, want once", alerts())
	}

	rpc.fees[testXLMSAC] = 0 // swept
	m.Check(context.Background())
	rpc.fees[testXLMSAC] = 12_000_000
	m.Check(context.Background())
	if alerts() != 2 {
		t.Fatalf("alerted %d times after a sweep and a new crossing, want 2", alerts())
	}
}

func TestCheckSkipsForwarderWhenForwardModeIsOff(t *testing.T) {
	rpc := &fakeRPC{balances: map[string]int64{}, fees: map[string]uint64{testXLMSAC: 99_000_000}}
	m, _, _ := newMonitor(t, rpc)
	m.Check(context.Background())
	if fees := m.Snapshot().ForwarderFees; fees != nil {
		t.Fatalf("forwarder fees read with forward mode off: %v", fees)
	}
}

package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/base"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/latch/relayer/internal/config"
)

// A deposit must be in the forwards table before the cursor moves past it.
// Otherwise a failed insert, or a crash while it waits in the queue, leaves the
// money in the pool with no record, and the stream never replays it.

type fakeStore struct {
	events    []string // "insert:<key>" and "cursor:<token>", in call order
	insertErr error
	dup       bool
	cursor    string
}

func (s *fakeStore) GetCursor(context.Context, string) (string, error) { return s.cursor, nil }
func (s *fakeStore) UpsertCursor(_ context.Context, _, cursor string) error {
	s.events = append(s.events, "cursor:"+cursor)
	s.cursor = cursor
	return nil
}
func (s *fakeStore) InsertForward(_ context.Context, txHash string, _ uint64, _, _, _, _ string, _ time.Time) (bool, error) {
	if s.insertErr != nil {
		return false, s.insertErr
	}
	s.events = append(s.events, "insert:"+txHash)
	return !s.dup, nil
}

type fakeStream struct{ ops []operations.Operation }

func (f *fakeStream) StreamPayments(ctx context.Context, _ horizonclient.OperationRequest, handler horizonclient.OperationHandler) error {
	for _, op := range f.ops {
		if ctx.Err() != nil {
			return nil
		}
		handler(op)
	}
	return nil
}

func deposit(txHash, token string) operations.Payment {
	return operations.Payment{
		Base: operations.Base{ID: token, PT: token, TransactionHash: txHash,
			Transaction: &horizon.Transaction{MemoType: "id", Memo: "7", OperationCount: 1}},
		Asset:  base.Asset{Type: "native"},
		From:   other,
		To:     pool,
		Amount: "1.0000000",
	}
}

func testWatcher(st *fakeStore, stream *fakeStream) *Watcher {
	return &Watcher{
		pool:    config.PoolAccount{Address: pool},
		store:   st,
		horizon: stream,
		jobs:    make(chan forwardJob, forwardQueue),
	}
}

func TestHandle_recordsBeforeSavingCursor(t *testing.T) {
	st := &fakeStore{}
	w := testWatcher(st, nil)

	if err := w.handle(context.Background(), deposit("dep-1", "100")); err != nil {
		t.Fatal(err)
	}
	if len(st.events) != 2 || st.events[0] != "insert:dep-1" || st.events[1] != "cursor:100" {
		t.Fatalf("calls = %v, want the insert before the cursor", st.events)
	}
	// Nothing ever takes the job off the queue, as if the process died now:
	// the row already exists for the retry worker to find.
	if len(w.jobs) != 1 {
		t.Fatalf("queued %d jobs, want 1", len(w.jobs))
	}
}

func TestStream_failedInsertStopsWithoutSavingCursor(t *testing.T) {
	st := &fakeStore{cursor: "99", insertErr: errors.New("connection refused")}
	stream := &fakeStream{ops: []operations.Operation{deposit("dep-1", "100"), deposit("dep-2", "101")}}
	w := testWatcher(st, stream)

	err := w.stream(context.Background())

	if err == nil {
		t.Fatal("stream returned no error after a deposit could not be recorded")
	}
	if st.cursor != "99" || len(st.events) != 0 {
		t.Fatalf("cursor = %s, calls = %v; want the cursor left at 99 so the deposit is replayed", st.cursor, st.events)
	}
	if len(w.jobs) != 0 {
		t.Fatalf("dispatched %d jobs for unrecorded deposits", len(w.jobs))
	}
}

func TestHandle_alreadyRecordedIsNotDispatchedAgain(t *testing.T) {
	st := &fakeStore{dup: true}
	w := testWatcher(st, nil)

	if err := w.handle(context.Background(), deposit("dep-1", "100")); err != nil {
		t.Fatal(err)
	}
	if len(w.jobs) != 0 {
		t.Fatal("a replayed deposit was dispatched again")
	}
	if st.cursor != "100" {
		t.Fatalf("cursor = %q, want it saved past the replay", st.cursor)
	}
}

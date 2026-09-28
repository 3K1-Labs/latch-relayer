package watcher

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// fakeStream serves ops as Horizon pages: records after the request's cursor,
// at most Limit of them. Every request is kept so tests can check paging.
type fakeStream struct {
	ops      []operations.Operation
	requests []horizonclient.OperationRequest
	at       []time.Time // when each request was made
	onEmpty  func()      // called when a page comes back empty, e.g. to stop the poll
}

func (f *fakeStream) Payments(req horizonclient.OperationRequest) (operations.OperationsPage, error) {
	f.requests = append(f.requests, req)
	f.at = append(f.at, time.Now())
	var page operations.OperationsPage
	if req.Order == horizonclient.OrderDesc {
		if n := len(f.ops); n > 0 {
			page.Embedded.Records = []operations.Operation{f.ops[n-1]}
		}
		return page, nil
	}
	after, _ := strconv.ParseInt(req.Cursor, 10, 64)
	for _, op := range f.ops {
		if tok, _ := strconv.ParseInt(op.PagingToken(), 10, 64); tok <= after {
			continue
		}
		if len(page.Embedded.Records) == int(req.Limit) {
			break
		}
		page.Embedded.Records = append(page.Embedded.Records, op)
	}
	if len(page.Embedded.Records) == 0 && f.onEmpty != nil {
		f.onEmpty()
	}
	return page, nil
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

func TestPoll_failedInsertStopsWithoutSavingCursor(t *testing.T) {
	st := &fakeStore{cursor: "99", insertErr: errors.New("connection refused")}
	stream := &fakeStream{ops: []operations.Operation{deposit("dep-1", "100"), deposit("dep-2", "101")}}
	w := testWatcher(st, stream)

	err := w.poll(context.Background())

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

// A backlog larger than one page is read page after page without waiting:
// Horizon's SSE stream delivered bursts at about 9 payments/s, which made
// intake the bottleneck once channels could forward 100 per ledger.
func TestPoll_drainsFullPagesWithoutWaiting(t *testing.T) {
	var ops []operations.Operation
	for i := range pageSize + 50 {
		ops = append(ops, deposit(fmt.Sprintf("dep-%d", i), strconv.Itoa(1000+i)))
	}
	st := &fakeStore{cursor: "999"}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeStream{ops: ops, onEmpty: cancel}
	w := testWatcher(st, stream)
	w.jobs = make(chan forwardJob, len(ops))

	if err := w.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if gap := stream.at[1].Sub(stream.at[0]); gap >= pollGap {
		t.Fatalf("the page after a full one was requested %s later; want at once", gap)
	}
	if len(w.jobs) != len(ops) {
		t.Fatalf("dispatched %d of %d deposits", len(w.jobs), len(ops))
	}
	if st.cursor != strconv.Itoa(1000+len(ops)-1) {
		t.Fatalf("cursor = %s, want the last payment's", st.cursor)
	}
	if len(stream.requests) < 2 || stream.requests[0].Limit != pageSize || stream.requests[1].Cursor != strconv.Itoa(1000+pageSize-1) {
		t.Fatalf("requests = %+v, want a full page then the next from its last token", stream.requests)
	}
}

// With no saved cursor, polling starts after the pool's latest payment (the
// paged "now"), not from the beginning of the pool's history.
func TestPoll_noCursorStartsFromLatest(t *testing.T) {
	st := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeStream{ops: []operations.Operation{deposit("old-1", "500"), deposit("old-2", "501")}, onEmpty: cancel}
	w := testWatcher(st, stream)

	if err := w.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.jobs) != 0 {
		t.Fatalf("replayed %d historical deposits", len(w.jobs))
	}
	if last := stream.requests[len(stream.requests)-1]; last.Cursor != "501" {
		t.Fatalf("polled from cursor %q, want the latest payment's (501)", last.Cursor)
	}
}

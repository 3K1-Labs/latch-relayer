package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/latch/relayer/internal/gasless/sponsor"
)

type fakeSponsor struct {
	prepErr  error
	existing *sponsor.Record
	result   sponsor.Record
	block    chan struct{} // Run waits on it when set
	ran      bool
}

func (f *fakeSponsor) Prepare(_ context.Context, req sponsor.Request) (sponsor.Prepared, bool, error) {
	if f.prepErr != nil {
		return sponsor.Prepared{}, false, f.prepErr
	}
	if f.existing != nil {
		return sponsor.Prepared{Record: *f.existing}, true, nil
	}
	return sponsor.Prepared{Record: sponsor.Record{RequestID: req.RequestID, Status: sponsor.StatusPending}}, false, nil
}

func (f *fakeSponsor) Run(context.Context, sponsor.Prepared) sponsor.Record {
	f.ran = true
	if f.block != nil {
		<-f.block
	}
	return f.result
}

type inline struct{}

func (inline) Go(f func(ctx context.Context)) { go f(context.Background()) }

type fakeReader map[string]sponsor.Record

func (r fakeReader) Get(_ context.Context, id string) (sponsor.Record, bool, error) {
	rec, ok := r[id]
	return rec, ok, nil
}

func serve(t *testing.T, s *Submissions, method, path, body string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

const body = `{"request_id":"req_00001","wallet":"C","mode":"sponsored","transaction":"AAAA"}`

func TestSubmit_ReturnsFinalOutcome(t *testing.T) {
	sp := &fakeSponsor{result: sponsor.Record{RequestID: "req_00001", Status: sponsor.StatusSuccess, TxHash: "abc"}}
	code, out := serve(t, &Submissions{Sponsor: sp, Work: inline{}, SyncWait: time.Second}, "POST", "/gasless/submit", body)
	if code != http.StatusOK || out["status"] != "success" || out["tx_hash"] != "abc" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestSubmit_SlowSubmissionAnswers202(t *testing.T) {
	sp := &fakeSponsor{block: make(chan struct{})}
	defer close(sp.block)
	code, out := serve(t, &Submissions{Sponsor: sp, Work: inline{}, SyncWait: 10 * time.Millisecond}, "POST", "/gasless/submit", body)
	if code != http.StatusAccepted || out["status"] != "pending" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestSubmit_RepeatDoesNotRunAgain(t *testing.T) {
	sp := &fakeSponsor{existing: &sponsor.Record{RequestID: "req_00001", Status: sponsor.StatusFailed}}
	code, out := serve(t, &Submissions{Sponsor: sp, Work: inline{}, SyncWait: time.Second}, "POST", "/gasless/submit", body)
	if code != http.StatusOK || out["status"] != "failed" || sp.ran {
		t.Fatalf("%d %v ran=%v", code, out, sp.ran)
	}
}

func TestSubmit_ErrorMapping(t *testing.T) {
	cases := map[error]struct {
		status int
		code   string
	}{
		sponsor.ErrInvalid:         {400, "invalid_request"},
		sponsor.ErrNotSponsorable:  {403, "not_sponsorable"},
		sponsor.ErrForwardNotBuilt: {501, "not_implemented"},
		sponsor.ErrConflict:        {409, "request_id_conflict"},
		sponsor.ErrWalletCap:       {429, "wallet_cap_reached"},
		sponsor.ErrDailyBudget:     {429, "daily_budget_reached"},
		sponsor.ErrSimulation:      {422, "simulation_failed"},
		sponsor.ErrUnavailable:     {503, "sponsorship_unavailable"},
	}
	for err, want := range cases {
		code, out := serve(t, &Submissions{Sponsor: &fakeSponsor{prepErr: err}, Work: inline{}, SyncWait: time.Second}, "POST", "/gasless/submit", body)
		e, _ := out["error"].(map[string]any)
		if code != want.status || e["code"] != want.code {
			t.Errorf("%v: got %d %v, want %d %s", err, code, e, want.status, want.code)
		}
	}
}

func TestSubmit_RejectsUnknownFields(t *testing.T) {
	code, _ := serve(t, &Submissions{Sponsor: &fakeSponsor{}, Work: inline{}, SyncWait: time.Second},
		"POST", "/gasless/submit", `{"request_id":"req_00001","fee":"0"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d", code)
	}
}

func TestGetRequest(t *testing.T) {
	s := &Submissions{Records: fakeReader{"req_00001": {RequestID: "req_00001", Status: sponsor.StatusUnconfirmed}}}
	if code, out := serve(t, s, "GET", "/gasless/requests/req_00001", ""); code != http.StatusAccepted || out["status"] != "unconfirmed" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := serve(t, s, "GET", "/gasless/requests/missing", ""); code != http.StatusNotFound {
		t.Fatalf("missing: %d", code)
	}
}

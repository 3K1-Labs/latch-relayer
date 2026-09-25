package forwarder

import (
	"context"
	"testing"
	"time"

	"github.com/latch/relayer/internal/store"
)

// The watcher records a deposit before queueing it, so a deposit can be seen by
// both a watcher worker and the retry worker. Only the one that claims the row
// may process it.

func TestForwardRecorded_claimedElsewhereDoesNothing(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st := forwarderWith(t, rpc)
	st.claimLost = true

	f.ForwardRecorded(context.Background(), f.config.PoolAccounts[0].Address, "in-taken", 1, "GABC", "10.0000000", "native", time.Now())

	if rpc.sent != 0 || len(rpc.simulated) != 0 || len(st.doneCalls) != 0 {
		t.Fatalf("processed a deposit the retry worker had claimed: sent=%d simulated=%d done=%d",
			rpc.sent, len(rpc.simulated), len(st.doneCalls))
	}
}

func TestForwardRecorded_claimsThenForwards(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st := forwarderWith(t, rpc)

	f.ForwardRecorded(context.Background(), f.config.PoolAccounts[0].Address, "in-mine", 1, "GABC", "10.0000000", "native", time.Now())

	if len(st.claimCalls) != 1 || st.claimCalls[0] != "in-mine" {
		t.Fatalf("claims = %v, want one for in-mine", st.claimCalls)
	}
	if len(st.doneCalls) != 1 {
		t.Fatalf("forward not completed: %v", st.markFailedCalls)
	}
}

func TestRetry_pendingClaimedByWorkerDoesNothing(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st := forwarderWith(t, rpc)
	st.claimLost = true

	f.Retry(context.Background(), store.Forward{
		TxHash: "in-queued", MemoID: 1, Amount: "10.0000000", Asset: "native",
		Status: store.StatusPending, PoolAddress: f.config.PoolAccounts[0].Address, UpdatedAt: time.Now(),
	})

	if rpc.sent != 0 || len(st.doneCalls) != 0 {
		t.Fatalf("retry processed a deposit a worker had claimed: sent=%d done=%d", rpc.sent, len(st.doneCalls))
	}
}

// A deposit recorded but never taken (the process died with it queued) is
// forwarded by the retry worker.
func TestRetry_orphanedPendingIsForwarded(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), store.Forward{
		TxHash: "in-orphan", MemoID: 1, Amount: "10.0000000", Asset: "native",
		Status: store.StatusPending, PoolAddress: f.config.PoolAccounts[0].Address, UpdatedAt: time.Now(),
	})

	if len(st.claimCalls) != 1 || len(st.doneCalls) != 1 {
		t.Fatalf("claims=%v done=%v; want the orphaned deposit claimed and forwarded", st.claimCalls, st.doneCalls)
	}
}

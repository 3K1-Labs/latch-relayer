package channels

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/latch/relayer/internal/gasless/keys"
	"github.com/latch/relayer/internal/testdb"
	"github.com/latch/relayer/migrations"
)

func setup(t *testing.T, count int) (*pgxpool.Pool, []keys.Channel) {
	t.Helper()
	db := testdb.New(t)
	if err := migrations.Run(context.Background(), db, migrations.Gasless); err != nil {
		t.Fatal(err)
	}
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	chans, err := keys.DeriveChannels(seed, count)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPool(db, "setup").Sync(context.Background(), chans); err != nil {
		t.Fatal(err)
	}
	return db, chans
}

// The core guarantee: many acquirers (standing in for many requests across
// many instances) never hold the same channel at once, and once every channel
// is out the rest get ErrPoolCapacity instead of blocking.
func TestConcurrentAcquireNeverDoubleLeases(t *testing.T) {
	db, _ := setup(t, 10)
	ctx := context.Background()

	var (
		mu       sync.Mutex
		held     = map[int]bool{}
		capacity int
		wg       sync.WaitGroup
	)
	for i := 0; i < 50; i++ {
		wg.Go(func() {
			l, err := NewPool(db, "instance").Acquire(ctx, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrPoolCapacity):
				capacity++
			case err != nil:
				t.Errorf("acquire: %v", err)
			case held[l.Index]:
				t.Errorf("channel %d leased twice", l.Index)
			default:
				held[l.Index] = true
			}
		})
	}
	wg.Wait()

	if len(held) != 10 || capacity != 40 {
		t.Fatalf("leased %d channels, %d got capacity errors; want 10 and 40", len(held), capacity)
	}
}

func TestReleaseRecordsSequenceAndFreesChannel(t *testing.T) {
	db, _ := setup(t, 1)
	ctx := context.Background()
	p := NewPool(db, "a")

	l, err := p.Acquire(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !l.NeedsResync {
		t.Fatal("a never-used channel must need a sequence resync")
	}
	if _, err := p.Acquire(ctx, time.Minute); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("second acquire on a 1-channel pool: %v, want ErrPoolCapacity", err)
	}

	seq := int64(4242)
	if err := p.Release(ctx, l, &seq, false); err != nil {
		t.Fatal(err)
	}
	l2, err := p.Acquire(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if l2.NeedsResync || l2.Seq != 4242 {
		t.Fatalf("after release: seq=%d resync=%v, want 4242/false", l2.Seq, l2.NeedsResync)
	}
}

// A crashed holder's lease expires; the next acquirer takes the channel and
// must resync (the dead holder may have consumed a sequence number), and the
// dead holder's late Release must not clobber the new lease.
func TestExpiredLeaseIsReclaimedWithResync(t *testing.T) {
	db, _ := setup(t, 1)
	ctx := context.Background()
	p := NewPool(db, "a")

	dead, err := p.Acquire(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	seq := int64(10)
	if err := p.Release(ctx, dead, &seq, false); err != nil {
		t.Fatal(err)
	}
	dead, _ = p.Acquire(ctx, time.Minute)
	if _, err := db.Exec(ctx, `UPDATE channel_accounts SET lease_expires_at = NOW() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}

	fresh, err := NewPool(db, "b").Acquire(ctx, time.Minute)
	if err != nil {
		t.Fatalf("expired lease not reclaimed: %v", err)
	}
	if !fresh.NeedsResync {
		t.Fatal("taking over an expired lease must force a resync")
	}
	if err := p.Release(ctx, dead, &seq, false); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale holder release: %v, want ErrLeaseLost", err)
	}
}

func TestSyncRetiresAndReactivates(t *testing.T) {
	db, chans := setup(t, 3)
	ctx := context.Background()
	p := NewPool(db, "a")

	if err := p.Sync(ctx, chans[:1]); err != nil {
		t.Fatal(err)
	}
	s, _ := p.Stats(ctx)
	if s.Active != 1 || s.Retired != 2 {
		t.Fatalf("after shrinking to 1: %+v", s)
	}

	if err := p.Sync(ctx, chans); err != nil {
		t.Fatal(err)
	}
	s, _ = p.Stats(ctx)
	if s.Active != 3 || s.Retired != 0 {
		t.Fatalf("after growing back to 3: %+v", s)
	}
}

func TestRecordBalanceMovesChannelsInAndOutOfRotation(t *testing.T) {
	db, _ := setup(t, 2)
	ctx := context.Background()
	p := NewPool(db, "a")
	const min = 10_000_000

	_ = p.RecordBalance(ctx, 0, -1, min)    // not found on network
	_ = p.RecordBalance(ctx, 1, min-1, min) // under floor
	if _, err := p.Acquire(ctx, time.Minute); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("disabled channels were leasable: %v", err)
	}

	_ = p.RecordBalance(ctx, 1, min, min)
	l, err := p.Acquire(ctx, time.Minute)
	if err != nil || l.Index != 1 {
		t.Fatalf("recovered channel not leasable: %+v %v", l, err)
	}
}

func TestAcquireWaitGetsChannelFreedMidWait(t *testing.T) {
	db, _ := setup(t, 1)
	ctx := context.Background()
	p := NewPool(db, "a")

	l, _ := p.Acquire(ctx, time.Minute)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = p.Release(ctx, l, nil, true)
	}()
	if _, err := p.AcquireWait(ctx, time.Minute, 2*time.Second); err != nil {
		t.Fatalf("AcquireWait: %v", err)
	}
	if _, err := p.AcquireWait(ctx, time.Minute, 100*time.Millisecond); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("AcquireWait on a busy pool: %v, want ErrPoolCapacity", err)
	}
}

package torrent

import (
	"context"
	"testing"
	"time"

	qt "github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/internal/testutil"
)

func TestReaderReadContext(t *testing.T) {
	cl, err := NewClient(TestingConfig(t))
	qt.Assert(t, qt.IsNil(err))
	defer cl.Close()
	tt, err := cl.AddTorrent(testutil.GreetingMetaInfo())
	qt.Assert(t, qt.IsNil(err))
	defer tt.Drop()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond))
	defer cancel()
	r := tt.Files()[0].NewReader()
	defer r.Close()
	_, err = r.ReadContext(ctx, make([]byte, 1))
	qt.Assert(t, qt.Equals(err, context.DeadlineExceeded))
}

func TestReaderSetContextAndRead(t *testing.T) {
	cl, err := NewClient(TestingConfig(t))
	qt.Assert(t, qt.IsNil(err))
	defer cl.Close()
	tt, err := cl.AddTorrent(testutil.GreetingMetaInfo())
	qt.Assert(t, qt.IsNil(err))
	defer tt.Drop()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond))
	defer cancel()
	r := tt.Files()[0].NewReader()
	defer r.Close()
	r.SetContext(ctx)
	_, err = r.Read(make([]byte, 1))
	qt.Assert(t, qt.Equals(err, context.DeadlineExceeded))
}

// TestShouldRetryStorageCapRead proves the storage-cap retry recursion in readAtAttempt is
// bounded: retries stop once maxStorageCapRetries is reached, and stop immediately if the
// caller's context is already done, regardless of how many attempts remain in the budget. This
// guards against the fork-local bug where readAt used to recurse via r.readAt(ctx, b, pos) with
// no bound whenever hasStorageCap() was true, hanging playback forever on a persistent storage
// error.
func TestShouldRetryStorageCapRead(t *testing.T) {
	ctx := context.Background()
	for attempt := range maxStorageCapRetries {
		retry, err := shouldRetryStorageCapRead(ctx, attempt)
		qt.Assert(t, qt.IsTrue(retry), qt.Commentf("attempt %d should still retry", attempt))
		qt.Assert(t, qt.IsNil(err))
	}
	// Once the budget is exhausted, no more retries, and no additional error is manufactured (the
	// caller keeps returning the last read error).
	retry, err := shouldRetryStorageCapRead(ctx, maxStorageCapRetries)
	qt.Assert(t, qt.IsFalse(retry), qt.Commentf("retry budget should be exhausted"))
	qt.Assert(t, qt.IsNil(err))
	// Retrying further (simulating a caller ignoring the cap) must stay false: this proves the
	// bound holds regardless of how far attempt overshoots, not just at the boundary.
	retry, err = shouldRetryStorageCapRead(ctx, maxStorageCapRetries+50)
	qt.Assert(t, qt.IsFalse(retry), qt.Commentf("retry budget must stay exhausted past the cap"))
	qt.Assert(t, qt.IsNil(err))

	// A caller that has already given up must stop the retry loop immediately, even on the very
	// first attempt, and surface ctx.Err() so readAtAttempt can return it.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	retry, err = shouldRetryStorageCapRead(cancelledCtx, 0)
	qt.Assert(t, qt.IsFalse(retry), qt.Commentf("cancelled context must stop retries"))
	qt.Assert(t, qt.Equals(err, context.Canceled))
}

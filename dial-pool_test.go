package torrent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-quicktest/qt"
)

// fakeDialer is a Dialer that never touches the network. It lets tests drive dialPool/DialFirst
// plumbing deterministically instead of relying on a live dial target.
type fakeDialer struct {
	network string
	conn    net.Conn
	err     error
}

func (f fakeDialer) Dial(_ context.Context, _ string) (net.Conn, error) {
	return f.conn, f.err
}

func (f fakeDialer) DialerNetwork() string { return f.network }

// callWithTimeout runs fn in a goroutine and fails the test instead of hanging forever if fn
// doesn't return in time. Used below because the bug under test is a permanent deadlock, not a
// panic: a plain call to DialFirst would hang the whole test run (and leak the test binary) on
// regression.
func callWithTimeout(t *testing.T, timeout time.Duration, fn func() DialResult) DialResult {
	t.Helper()
	done := make(chan DialResult, 1)
	go func() { done <- fn() }()
	select {
	case res := <-done:
		return res
	case <-time.After(timeout):
		t.Fatal("call did not return within timeout; likely blocked on a nil dialPool.resCh channel")
		return DialResult{}
	}
}

// TestDialFirstNoDeadlockOnAllFailures is a regression test for DialFirst constructing its
// dialPool literal without initializing resCh. dialPool.add sends results on resCh from a
// goroutine and dialPool.getFirst/drainAndCloseRemainingDials receive from it; with a nil resCh,
// both the send and the receive block forever (send/receive on a nil channel never proceeds),
// hanging DialFirst permanently and leaking one goroutine per dialer. This drives DialFirst with
// dialers that all fail immediately, so getFirst must fully drain resCh to return.
func TestDialFirstNoDeadlockOnAllFailures(t *testing.T) {
	dialers := []Dialer{
		fakeDialer{network: "tcp", err: errors.New("fake dial error 1")},
		fakeDialer{network: "utp", err: errors.New("fake dial error 2")},
	}
	res := callWithTimeout(t, 5*time.Second, func() DialResult {
		return DialFirst(context.Background(), "203.0.113.1:1", dialers)
	})
	qt.Assert(t, qt.IsNil(res.Conn), qt.Commentf("no dialer succeeded, so DialFirst should return a nil Conn"))
}

// TestDialFirstReturnsFirstSuccessfulConn exercises the same DialFirst/dialPool.add/getFirst path
// as above but with one dialer that succeeds, confirming DialFirst still correctly surfaces a
// winning connection once resCh is a real (unbuffered, matching the working call site in
// dialAndCompleteHandshake) channel rather than nil.
func TestDialFirstReturnsFirstSuccessfulConn(t *testing.T) {
	succConn, otherEnd := net.Pipe()
	defer otherEnd.Close()

	dialers := []Dialer{
		fakeDialer{network: "tcp", err: errors.New("fake dial error")},
		fakeDialer{network: "utp", conn: succConn},
	}
	res := callWithTimeout(t, 5*time.Second, func() DialResult {
		return DialFirst(context.Background(), "203.0.113.1:1", dialers)
	})
	qt.Assert(t, qt.IsNotNil(res.Conn))
	qt.Assert(t, qt.Equals[net.Conn](res.Conn, succConn))
	res.Conn.Close()
}

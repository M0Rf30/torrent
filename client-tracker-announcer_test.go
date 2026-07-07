//go:build go1.25

package torrent

import (
	"errors"
	"log/slog"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anacrolix/missinggo/v2/panicif"
	"github.com/anacrolix/torrent/tracker"
	"github.com/go-quicktest/qt"
)

// This test doesn't really do much useful anymore. It is useful to break apart the dispatcher a bit
// for testing. It's good to have something that hits up the triggers a bit.
func TestUpdateOverdueRecursion(t *testing.T) {
	// Prevent synctest from tracking some stuff that we don't care about.
	cl := newTestingClient(t)
	synctest.Test(t, func(t *testing.T) {
		d := regularTrackerAnnounceDispatcher{}
		d.initTables()
		d.initTimerNoop()
		d.logger = slog.Default()
		u, _ := url.Parse("http://derp")
		d.initTrackerClient(u, trackerAnnouncerKey(u.String()), cl.config, slog.Default())
		// Two values. One that needs to be marked not overdue on the first call to updateOverdue,
		// and the other that is by a recursive call, and subsequently reversed when we bounce back
		// out to the original call.
		key1 := torrentTrackerAnnouncerKey{}
		key1.ShortInfohash[0] = 1
		key2 := torrentTrackerAnnouncerKey{}
		key2.ShortInfohash[0] = 2
		value1 := nextAnnounceInput{}
		value1.overdue = false
		value1.When = time.Now()
		value2 := nextAnnounceInput{}
		value2.overdue = true
		value2.When = time.Now().Add(4)
		println(value1.When.UnixNano(), value2.When.UnixNano())
		panicif.False(d.announceData.Create(key1, value1))
		panicif.False(d.announceData.Create(key2, value2))
		v2, ok := d.announceData.Get(key2)
		panicif.False(ok)
		expectedValue2 := value2
		expectedValue2.overdue = false
		qt.Check(t, qt.Equals(v2, expectedValue2))
		println(time.Now().UnixNano())
		// This will fix up the values. But if we can advance time and trigger a recursive
		// updateOverdue we can test for thrashing, but it's non-trivial.
		d.updateOverdue()
	})
}

func TestReAddForcesFreshStartedAnnounce(t *testing.T) {
	cl := &Client{
		config:              &ClientConfig{TorrentPeersLowWater: 50},
		torrents:            make(map[*Torrent]struct{}),
		torrentsByShortHash: make(map[shortInfohash]*Torrent),
	}
	tor := &Torrent{cl: cl}
	cl.torrents[tor] = struct{}{}

	var key torrentTrackerAnnouncerKey
	key.ShortInfohash[0] = 1
	key.url = trackerAnnouncerKey("http://tracker.example/announce")
	cl.torrentsByShortHash[key.ShortInfohash] = tor

	d := regularTrackerAnnounceDispatcher{torrentClient: cl, logger: slog.Default()}
	d.initTables()
	d.initTimerNoop()
	u, err := url.Parse(string(key.url))
	qt.Assert(t, qt.IsNil(err))
	d.initTrackerClient(u, key.url, cl.config, slog.Default())
	d.announceStates = map[torrentTrackerAnnouncerKey]*announceState{
		key: {
			lastOk: lastAnnounceOk{
				AnnouncedEvent: tracker.None,
				Completed:      time.Now(),
				Interval:       time.Hour,
			},
			Err:                  errors.New("stale announce error"),
			lastAttemptCompleted: time.Now(),
			sentCompleted:        true,
		},
	}
	d.resetAnnounceStateForReadd(key)

	event, when := d.nextAnnounceEvent(key)
	qt.Assert(t, qt.Equals(event, tracker.Started))
	qt.Assert(t, qt.IsFalse(when.IsZero()))
	state := d.announceStates[key]
	qt.Assert(t, qt.IsFalse(state.sentCompleted))
	qt.Assert(t, qt.IsNil(state.Err))
}

// Regression test: if a torrent is dropped right after its last announce attempt for a tracker
// errored, nextAnnounceEvent must still schedule a best-effort Stopped event instead of silently
// discarding the announce state, which would leave stale swarm membership on the tracker (BEP 3
// requires a Stopped event on leaving). It must also not retry indefinitely if that Stopped
// attempt itself fails.
func TestDroppedTorrentWithErroredAnnounceStillSendsStopped(t *testing.T) {
	cl := &Client{
		config:              &ClientConfig{TorrentPeersLowWater: 50},
		torrents:            make(map[*Torrent]struct{}),
		torrentsByShortHash: make(map[shortInfohash]*Torrent),
	}
	// The torrent is deliberately absent from cl.torrentsByShortHash, simulating that it has
	// already been dropped from the client.

	var key torrentTrackerAnnouncerKey
	key.ShortInfohash[0] = 1
	key.url = trackerAnnouncerKey("http://tracker.example/announce")

	d := regularTrackerAnnounceDispatcher{torrentClient: cl, logger: slog.Default()}
	d.initTables()
	d.initTimerNoop()
	u, err := url.Parse(string(key.url))
	qt.Assert(t, qt.IsNil(err))
	d.initTrackerClient(u, key.url, cl.config, slog.Default())

	d.announceStates = map[torrentTrackerAnnouncerKey]*announceState{
		key: {
			// We successfully joined the swarm previously...
			lastOk: lastAnnounceOk{
				AnnouncedEvent: tracker.Started,
				Completed:      time.Now().Add(-time.Hour),
				Interval:       time.Minute,
			},
			// ...but the most recent announce attempt (e.g. a keep-alive renewal) failed.
			Err:                  errors.New("tracker unreachable"),
			lastAttemptCompleted: time.Now(),
			lastAttemptEvent:     tracker.None,
		},
	}

	event, when := d.nextAnnounceEvent(key)
	qt.Assert(t, qt.Equals(event, tracker.Stopped),
		qt.Commentf("dropping a torrent after an errored announce must still attempt a best-effort Stopped event"))
	qt.Assert(t, qt.IsFalse(when.IsZero()))

	// Now simulate that the best-effort Stopped attempt itself failed, as singleAnnounce would
	// record it. We must not retry Stopped indefinitely for a torrent that's already gone.
	state := d.announceStates[key]
	state.Err = errors.New("tracker still unreachable")
	state.lastAttemptEvent = tracker.Stopped
	state.lastAttemptCompleted = time.Now()

	event, when = d.nextAnnounceEvent(key)
	qt.Assert(t, qt.Equals(event, tracker.None),
		qt.Commentf("must give up, not retry indefinitely, once a Stopped attempt has already failed"))
	qt.Assert(t, qt.IsTrue(when.IsZero()))
}

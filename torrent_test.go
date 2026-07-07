package torrent

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/missinggo/v2"
	"github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/anacrolix/torrent/storage"
)

func r(i, b, l pp.Integer) Request {
	return Request{i, ChunkSpec{b, l}}
}

// Check the given request is correct for various torrent offsets.
func TestTorrentRequest(t *testing.T) {
	const s = 472183431 // Length of torrent.
	for _, _case := range []struct {
		off int64   // An offset into the torrent.
		req Request // The expected request. The zero value means !ok.
	}{
		// Invalid offset.
		{-1, Request{}},
		{0, r(0, 0, 16384)},
		// One before the end of a piece.
		{1<<18 - 1, r(0, 1<<18-16384, 16384)},
		// Offset beyond torrent length.
		{472 * 1 << 20, Request{}},
		// One before the end of the torrent. Complicates the chunk length.
		{s - 1, r((s-1)/(1<<18), (s-1)%(1<<18)/(16384)*(16384), 12935)},
		{1, r(0, 0, 16384)},
		// One before end of chunk.
		{16383, r(0, 0, 16384)},
		// Second chunk.
		{16384, r(0, 16384, 16384)},
	} {
		req, ok := torrentOffsetRequest(472183431, 1<<18, 16384, _case.off)
		if (_case.req == Request{}) == ok {
			t.Fatalf("expected %v, got %v", _case.req, req)
		}
		if req != _case.req {
			t.Fatalf("expected %v, got %v", _case.req, req)
		}
	}
}

func TestAppendToCopySlice(t *testing.T) {
	orig := []int{1, 2, 3}
	dupe := append([]int{}, orig...)
	dupe[0] = 4
	if orig[0] != 1 {
		t.FailNow()
	}
}

func TestTorrentString(t *testing.T) {
	tor := &Torrent{}
	tor.infoHash.Ok = true
	tor.infoHash.Value[0] = 1
	s := tor.InfoHash().HexString()
	if s != "0100000000000000000000000000000000000000" {
		t.FailNow()
	}
}

// This benchmark is from the observation that a lot of overlapping Readers on
// a large torrent with small pieces had a lot of overhead in recalculating
// piece priorities everytime a reader (possibly in another Torrent) changed.
func BenchmarkUpdatePiecePriorities(b *testing.B) {
	const (
		numPieces   = 13410
		pieceLength = 256 << 10
	)
	cl := newTestingClient(b)
	t := cl.newTorrentForTesting()
	qt.Assert(b, qt.IsNil(t.setInfoUnlocked(&metainfo.Info{
		Pieces:      make([]byte, metainfo.HashSize*numPieces),
		PieceLength: pieceLength,
		Length:      pieceLength * numPieces,
	})))
	qt.Check(b, qt.Equals(t.numPieces(), 13410))
	for i := 0; i < 7; i += 1 {
		r := t.NewReader()
		r.SetReadahead(32 << 20)
		r.Seek(3500000, io.SeekStart)
	}
	qt.Check(b, qt.HasLen(t.readers, 7))
	for i := 0; i < t.numPieces(); i += 3 {
		t._completedPieces.Add(i)
	}
	t.DownloadPieces(0, t.numPieces())
	for b.Loop() {
		cl.lock()
		t.updateAllPiecePriorities("")
		cl.unlock()
	}
}

// Check that a torrent containing zero-length file(s) will start, and that
// they're created in the filesystem. The client storage is assumed to be
// file-based on the native filesystem based.
func testEmptyFilesAndZeroPieceLength(t *testing.T, cfg *ClientConfig) {
	cl, err := NewClient(cfg)
	qt.Assert(t, qt.IsNil(err))
	defer cl.Close()
	ib, err := bencode.Marshal(metainfo.Info{
		Name:        "empty",
		Length:      0,
		PieceLength: 0,
	})
	qt.Assert(t, qt.IsNil(err))
	fp := filepath.Join(cfg.DataDir, "empty")
	os.Remove(fp)
	qt.Check(t, qt.IsFalse(missinggo.FilePathExists(fp)))
	tt, err := cl.AddTorrent(&metainfo.MetaInfo{
		InfoBytes: ib,
	})
	qt.Assert(t, qt.IsNil(err))
	defer tt.Drop()
	tt.DownloadAll()
	qt.Assert(t, qt.IsTrue(cl.WaitAll()))
	qt.Check(t, qt.IsTrue(tt.Complete().Bool()))
	qt.Check(t, qt.IsTrue(missinggo.FilePathExists(fp)))
}

func TestEmptyFilesAndZeroPieceLengthWithFileStorage(t *testing.T) {
	cfg := TestingConfig(t)
	ci := storage.NewFile(cfg.DataDir)
	defer ci.Close()
	cfg.DefaultStorage = ci
	testEmptyFilesAndZeroPieceLength(t, cfg)
}

func TestPieceHashFailed(t *testing.T) {
	mi := testutil.GreetingMetaInfo()
	cl := newTestingClient(t)
	tt := cl.newTorrent(mi.HashInfoBytes(), badStorage{})
	tt.setChunkSize(2)
	tt.cl.lock()
	qt.Assert(t, qt.IsNil(tt.setInfoBytesLocked(mi.InfoBytes)))
	tt.cl.unlock()
	tt.cl.lock()
	tt.dirtyChunks.AddRange(
		uint64(tt.pieceRequestIndexBegin(1)),
		uint64(tt.pieceRequestIndexBegin(1)+3))
	qt.Assert(t, qt.IsTrue(tt.pieceAllDirty(1)))
	tt.pieceHashed(1, false, nil)
	// Dirty chunks should be cleared so we can try again.
	qt.Assert(t, qt.IsFalse(tt.pieceAllDirty(1)))
	tt.cl.unlock()
}

// Check the behaviour of Torrent.Metainfo when metadata is not completed.
func TestTorrentMetainfoIncompleteMetadata(t *testing.T) {
	cfg := TestingConfig(t)
	cfg.Debug = true
	// Disable this just because we manually initiate a connection without it.
	cfg.MinPeerExtensions.SetBit(pp.ExtensionBitFast, false)
	cl, err := NewClient(cfg)
	qt.Assert(t, qt.IsNil(err))
	defer cl.Close()

	mi := testutil.GreetingMetaInfo()
	ih := mi.HashInfoBytes()

	tt, _ := cl.AddTorrentInfoHash(ih)
	qt.Check(t, qt.IsNil(tt.Metainfo().InfoBytes))
	qt.Check(t, qt.IsFalse(tt.haveAllMetadataPieces()))

	nc, err := net.Dial("tcp", fmt.Sprintf(":%d", cl.LocalPort()))
	qt.Assert(t, qt.IsNil(err))
	defer nc.Close()

	var pex PeerExtensionBits
	pex.SetBit(pp.ExtensionBitLtep, true)
	hr, err := pp.Handshake(context.Background(), nc, &ih, [20]byte{}, pex)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.IsTrue(hr.PeerExtensionBits.GetBit(pp.ExtensionBitLtep)))
	qt.Check(t, qt.Equals(hr.PeerID, cl.PeerID()))
	qt.Check(t, qt.Equals(hr.Hash, ih))

	qt.Check(t, qt.Equals(tt.metadataSize(), 0))

	func() {
		cl.lock()
		defer cl.unlock()
		go func() {
			_, err = nc.Write(pp.Message{
				Type:       pp.Extended,
				ExtendedID: pp.HandshakeExtendedID,
				ExtendedPayload: func() []byte {
					d := map[string]interface{}{
						"metadata_size": len(mi.InfoBytes),
					}
					b, err := bencode.Marshal(d)
					if err != nil {
						panic(err)
					}
					return b
				}(),
			}.MustMarshalBinary())
			qt.Assert(t, qt.IsNil(err))
		}()
		tt.metadataChanged.Wait()
	}()
	qt.Check(t, qt.DeepEquals(tt.metadataBytes, make([]byte, len(mi.InfoBytes))))
	qt.Check(t, qt.IsFalse(tt.haveAllMetadataPieces()))
	qt.Check(t, qt.IsNil(tt.Metainfo().InfoBytes))
}

func TestRelativeAvailabilityHaveNone(t *testing.T) {
	var err error
	cl := newTestingClient(t)
	mi, _ := testutil.Greeting.Generate(5)
	tt := cl.newTorrentOpt(AddTorrentOpts{InfoHash: mi.HashInfoBytes()})
	tt.setChunkSize(2)
	g.MakeMapIfNil(&tt.conns)
	pc := PeerConn{}
	pc.t = tt
	pc.legacyPeerImpl = &pc
	pc.initRequestState()
	g.InitNew(&pc.callbacks)
	tt.cl.lock()
	tt.conns[&pc] = struct{}{}
	err = pc.peerSentHave(0)
	tt.cl.unlock()
	qt.Assert(t, qt.IsNil(err))
	err = tt.SetInfoBytes(mi.InfoBytes)
	qt.Assert(t, qt.IsNil(err))
	tt.cl.lock()
	err = pc.peerSentHaveNone()
	tt.cl.unlock()
	qt.Assert(t, qt.IsNil(err))
	tt.Drop()
	tt.assertAllPiecesRelativeAvailabilityZero()
}

// Fork-local fix (see CHANGELOG.md): a single transient piece-completion
// storage error must not permanently disable a torrent's data download -
// only a streak of pieceCompletionErrorDisableThreshold CONSECUTIVE errors
// should. nextCompletionErrorStreak is the pure decision function extracted
// from setCachedPieceCompletionFromStorage specifically so this doesn't
// require constructing a full Torrent/Client/storage stack to test.
func TestNextCompletionErrorStreak(t *testing.T) {
	qt.Assert(t, qt.Equals(pieceCompletionErrorDisableThreshold, 3),
		qt.Commentf("test below is written against this specific threshold"))

	streak := 0
	var disable bool

	streak, disable = nextCompletionErrorStreak(streak)
	qt.Assert(t, qt.Equals(streak, 1))
	qt.Assert(t, qt.IsFalse(disable), qt.Commentf("a single error must not disable"))

	streak, disable = nextCompletionErrorStreak(streak)
	qt.Assert(t, qt.Equals(streak, 2))
	qt.Assert(t, qt.IsFalse(disable), qt.Commentf("two consecutive errors still below threshold"))

	streak, disable = nextCompletionErrorStreak(streak)
	qt.Assert(t, qt.Equals(streak, 3))
	qt.Assert(t, qt.IsTrue(disable), qt.Commentf("threshold reached: safety net must trip"))

	// A persistently broken storage backend keeps tripping on every
	// subsequent call (never gets "stuck" below threshold).
	streak, disable = nextCompletionErrorStreak(streak)
	qt.Assert(t, qt.Equals(streak, 4))
	qt.Assert(t, qt.IsTrue(disable))
}

// Any successful completion check resets the streak - this is
// setCachedPieceCompletionFromStorage's responsibility (it sets
// t.completionErrorStreak = 0 on the non-error branch), not
// nextCompletionErrorStreak's; documented here since it's the behavior
// that actually makes the fix "self-healing" rather than just "slower to
// trip".
func TestNextCompletionErrorStreakResetsFromZero(t *testing.T) {
	// Simulates: 2 errors (below threshold), then a success resets the
	// caller's streak variable to 0, then 2 more errors - still below
	// threshold, proving the count didn't carry over.
	streak, disable := nextCompletionErrorStreak(0)
	streak, disable = nextCompletionErrorStreak(streak)
	qt.Assert(t, qt.Equals(streak, 2))
	qt.Assert(t, qt.IsFalse(disable))

	streak = 0 // what setCachedPieceCompletionFromStorage does on success

	streak, disable = nextCompletionErrorStreak(streak)
	streak, disable = nextCompletionErrorStreak(streak)
	qt.Assert(t, qt.Equals(streak, 2))
	qt.Assert(t, qt.IsFalse(disable), qt.Commentf("streak must not have carried over the reset"))
}

// Fork-local fix (see CHANGELOG.md): pieceHashed used to call
// p.Storage().MarkComplete(), log any error, and then unconditionally proceed to mark
// the piece complete in memory anyway - so a storage write failure left memory falsely
// believing a piece was persisted as complete when storage never recorded it, and
// nothing ever re-synced the divergence. badStorage.MarkComplete always errors, so this
// simulates that failure and proves the piece is instead treated like a failed hash
// check: not marked complete, and returned to a re-downloadable (not all-dirty) state,
// rather than getting stuck in an unrecoverable "believed complete but isn't" limbo.
func TestPieceHashPassedMarkCompleteError(t *testing.T) {
	mi := testutil.GreetingMetaInfo()
	cl := newTestingClient(t)
	tt := cl.newTorrent(mi.HashInfoBytes(), badStorage{})
	tt.setChunkSize(2)
	tt.cl.lock()
	qt.Assert(t, qt.IsNil(tt.setInfoBytesLocked(mi.InfoBytes)))
	tt.cl.unlock()
	tt.cl.lock()
	defer tt.cl.unlock()
	// Force a known starting state (incomplete), overriding what badStorage's
	// always-{Ok:true,Complete:true} Completion() would otherwise seed via the initial
	// piece check triggered by setInfoBytesLocked above.
	tt.setPieceCompletion(1, g.Some(false))
	tt.dirtyChunks.AddRange(
		uint64(tt.pieceRequestIndexBegin(1)),
		uint64(tt.pieceRequestIndexBegin(1)+3))
	qt.Assert(t, qt.IsTrue(tt.pieceAllDirty(1)))
	qt.Assert(t, qt.IsFalse(tt.pieceComplete(1)), qt.Commentf("piece must start out incomplete"))

	// badStorage.MarkComplete always fails; a hash pass must not be believed despite that.
	tt.pieceHashed(1, true, nil)

	qt.Assert(t, qt.IsFalse(tt.pieceComplete(1)),
		qt.Commentf("MarkComplete failed - the piece must not be recorded complete in memory"))
	// Dirty chunks should be cleared so the piece is redownloaded and re-verified,
	// naturally retrying MarkComplete, instead of sitting complete-in-storage-terms (all
	// chunks written) but permanently unmarked and unrequestable.
	qt.Assert(t, qt.IsFalse(tt.pieceAllDirty(1)))
}

// Fork-local fix (see CHANGELOG.md): companion to TestPieceHashPassedMarkCompleteError,
// covering the mirror MarkNotComplete path. Unlike the MarkComplete case, pieceHashed's
// unconditional setPieceCompletion(false) call on a failed hash check was deliberately
// NOT changed to skip or re-derive the flip (see the comment above it in torrent.go): a
// failed hash check must always be believed incomplete in memory regardless of whether
// persisting that fact to storage succeeded, since the alternative - re-deriving from
// storage.Completion(), which badStorage (like a storage backend whose not-complete
// write silently failed to land) always misreports as still complete - would let a
// torrent keep trusting data it just proved was corrupt.
func TestPieceHashFailedMarkNotCompleteErrorDoesNotResurrectComplete(t *testing.T) {
	mi := testutil.GreetingMetaInfo()
	cl := newTestingClient(t)
	tt := cl.newTorrent(mi.HashInfoBytes(), badStorage{})
	tt.setChunkSize(2)
	tt.cl.lock()
	qt.Assert(t, qt.IsNil(tt.setInfoBytesLocked(mi.InfoBytes)))
	tt.cl.unlock()
	tt.cl.lock()
	defer tt.cl.unlock()
	// Seed the cache as "confirmed complete", simulating a piece storage believed was
	// done before this (re-)verification found it corrupt - the dangerous direction for
	// a stale storage.Completion() read to resurrect.
	tt.setPieceCompletion(1, g.Some(true))
	qt.Assert(t, qt.IsTrue(tt.pieceComplete(1)))
	tt.dirtyChunks.AddRange(
		uint64(tt.pieceRequestIndexBegin(1)),
		uint64(tt.pieceRequestIndexBegin(1)+3))

	// badStorage.MarkNotComplete always fails, and its Completion() always claims
	// {Ok: true, Complete: true} regardless - the worst case for a naive "re-read
	// storage" fix, since it would resurrect the piece as complete.
	tt.pieceHashed(1, false, nil)

	qt.Assert(t, qt.IsFalse(tt.pieceComplete(1)),
		qt.Commentf("a failed hash check must never leave the piece marked complete, "+
			"even though MarkNotComplete failed and Completion() still (falsely) claims complete"))
}

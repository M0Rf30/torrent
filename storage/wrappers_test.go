package storage

import (
	"errors"
	"expvar"
	"io"
	"testing"

	"github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/metainfo"
)

// fakeReadAtPiece is a minimal PieceImpl for exercising Piece.ReadAt in isolation, without
// wiring up a full storage backend. Only ReadAt and MarkNotComplete are meaningful for these
// tests; the rest are unused stubs.
type fakeReadAtPiece struct {
	readAt          func(b []byte, off int64) (int, error)
	markNotComplete func() error
}

func (f fakeReadAtPiece) ReadAt(b []byte, off int64) (int, error)  { return f.readAt(b, off) }
func (f fakeReadAtPiece) WriteAt(b []byte, off int64) (int, error) { return len(b), nil }
func (f fakeReadAtPiece) MarkComplete() error                      { return nil }
func (f fakeReadAtPiece) MarkNotComplete() error                   { return f.markNotComplete() }
func (f fakeReadAtPiece) Completion() Completion                   { return Completion{Ok: true} }

var _ PieceImpl = fakeReadAtPiece{}

// testSinglePieceInfo builds the minimal single-file, single-piece v1 Info needed for
// metainfo.Piece.Length() to work, without touching a real storage backend.
func testSinglePieceInfo(length int64) *metainfo.Info {
	return &metainfo.Info{
		PieceLength: length,
		Length:      length,
		Pieces:      make([]byte, metainfo.HashSize), // exactly 1 piece; content is unused here.
	}
}

// expvarIntValue reads the current value of a packageExpvarMap counter, treating an unset key
// as 0, so tests can assert on the delta a call produces.
func expvarIntValue(key string) int64 {
	v := packageExpvarMap.Get(key)
	if v == nil {
		return 0
	}
	iv, ok := v.(*expvar.Int)
	if !ok {
		return 0
	}
	return iv.Value()
}

// Fork-local fix (see CHANGELOG.md): ReadAt used to call MarkNotComplete on a premature
// (before piece end) EOF and discard its returned error entirely - not even logged. This is
// the "TODO: Hey, this guy over here isn't checking errors" spot. There is no logger reachable
// from this generic wrapper for an arbitrary PieceImpl (unlike e.g. the file storage backend),
// and the identity of the io.EOF ReadAt returns is load-bearing for callers upstream
// (io.Copy/io.SectionReader in Piece.WriteTo, and reader.go's readAtAttempt) that compare
// against it with == rather than errors.Is - wrapping or joining it would silently turn a
// handled, expected short-read signal into an unhandled hard error for those callers. This
// test proves the fix instead makes the swallowed error observable via the package's existing
// expvar counter, without changing what ReadAt returns.
func TestReadAtPrematureEOFMarkNotCompleteErrorIsCounted(t *testing.T) {
	before := expvarIntValue("readAtMarkNotCompleteErrors")

	fake := fakeReadAtPiece{
		// Simulate a premature EOF: only 5 of the requested (and available, per piece length)
		// 10 bytes came back.
		readAt: func(b []byte, off int64) (int, error) {
			return 5, io.EOF
		},
		markNotComplete: func() error {
			return errors.New("simulated MarkNotComplete failure")
		},
	}
	p := Piece{PieceImpl: fake, mip: testSinglePieceInfo(10).Piece(0)}

	b := make([]byte, 10)
	n, err := p.ReadAt(b, 0)

	qt.Assert(t, qt.Equals(n, 5))
	qt.Assert(t, qt.IsTrue(err == io.EOF),
		qt.Commentf("MarkNotComplete's error must not replace or wrap the returned io.EOF - "+
			"callers upstream compare with == (io.Copy/io.SectionReader, reader.go's "+
			"readAtAttempt), not errors.Is, so identity must be preserved exactly"))
	qt.Assert(t, qt.Equals(expvarIntValue("readAtMarkNotCompleteErrors"), before+1),
		qt.Commentf("the swallowed MarkNotComplete error must be observable, not silently dropped"))
}

// Companion to TestReadAtPrematureEOFMarkNotCompleteErrorIsCounted: a successful
// MarkNotComplete on the same premature-EOF path must not bump the error counter.
func TestReadAtPrematureEOFMarkNotCompleteSuccessNotCounted(t *testing.T) {
	before := expvarIntValue("readAtMarkNotCompleteErrors")

	fake := fakeReadAtPiece{
		readAt: func(b []byte, off int64) (int, error) {
			return 5, io.EOF
		},
		markNotComplete: func() error {
			return nil
		},
	}
	p := Piece{PieceImpl: fake, mip: testSinglePieceInfo(10).Piece(0)}

	b := make([]byte, 10)
	n, err := p.ReadAt(b, 0)

	qt.Assert(t, qt.Equals(n, 5))
	qt.Assert(t, qt.IsTrue(err == io.EOF))
	qt.Assert(t, qt.Equals(expvarIntValue("readAtMarkNotCompleteErrors"), before))
}

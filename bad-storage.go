package torrent

import (
	"context"
	"errors"
	"math/rand"
	"strings"

	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type badStorage struct{}

var _ storage.ClientImpl = badStorage{}

func (bs badStorage) OpenTorrent(
	context.Context,
	*metainfo.Info,
	metainfo.Hash,
) (storage.TorrentImpl, error) {
	capFunc := func() (cap int64, capped bool) {
		return -1, true
	}
	return storage.TorrentImpl{
		Piece:    bs.Piece,
		Capacity: &capFunc,
	}, nil
}

func (bs badStorage) Piece(p metainfo.Piece) storage.PieceImpl {
	return badStoragePiece{p}
}

type badStoragePiece struct {
	p metainfo.Piece
}

var _ storage.PieceImpl = badStoragePiece{}

func (p badStoragePiece) WriteAt(b []byte, off int64) (int, error) {
	return 0, nil
}

func (p badStoragePiece) Completion() storage.Completion {
	return storage.Completion{Complete: true, Ok: true}
}

func (p badStoragePiece) MarkComplete() error {
	return errors.New("psyyyyyyyche")
}

func (p badStoragePiece) MarkNotComplete() error {
	return errors.New("psyyyyyyyche")
}

// randomlyTruncatedDataString simulates flaky-but-recovering storage: most calls return the
// full content, but a minority independently truncate it, so a read at any offset converges to
// success within a handful of attempts (matching reader.go's bounded maxStorageCapRetries retry
// budget) instead of requiring an unbounded number of retries. The prior version drew a uniform
// truncation length on every call (never full length), which made success for anything but the
// very start of the piece a low-probability event per attempt (about 1/14 for the last byte) -
// fine under the old unbounded-recursion behavior, but flaky/prone to spurious failure now that
// storage-cap retries are capped (fork-local fix, see CHANGELOG.md).
func (p badStoragePiece) randomlyTruncatedDataString() string {
	if rand.Intn(4) == 0 {
		return testutil.GreetingFileContents[:rand.Intn(14)]
	}
	return testutil.GreetingFileContents
}

func (p badStoragePiece) ReadAt(b []byte, off int64) (n int, err error) {
	r := strings.NewReader(p.randomlyTruncatedDataString())
	return r.ReadAt(b, off+p.p.Offset())
}

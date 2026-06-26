package codec

import (
	"io"

	"github.com/pierrec/lz4/v4"
)

// NewLz4Decoder returns a reader that lz4-decompresses from r. lz4 is read-
// only in ferry: we only support decoding the legacy single-file snapshots.
//
// The returned ReadCloser's Close is a no-op; lz4.Reader doesn't hold
// resources beyond the wrapped reader.
func NewLz4Decoder(r io.Reader) (io.ReadCloser, error) {
	return lz4ReaderCloser{lz4.NewReader(r)}, nil
}

type lz4ReaderCloser struct {
	*lz4.Reader
}

func (l lz4ReaderCloser) Close() error { return nil }

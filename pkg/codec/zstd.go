package codec

import (
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

const DefaultZstdLevel = 13

// NewZstdEncoder returns a writer that zstd-compresses to w at the given level
// using the requested number of encoder threads (0 = library default).
//
// Close on the returned writer flushes the zstd stream but does not close w.
func NewZstdEncoder(w io.Writer, level, threads int) (io.WriteCloser, error) {
	opts := []zstd.EOption{
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
	}
	if threads > 0 {
		opts = append(opts, zstd.WithEncoderConcurrency(threads))
	}
	enc, err := zstd.NewWriter(w, opts...)
	if err != nil {
		return nil, fmt.Errorf("zstd encoder: %w", err)
	}
	return enc, nil
}

// NewZstdDecoder returns a reader that zstd-decompresses from r.
// The returned reader's Close releases the decoder.
func NewZstdDecoder(r io.Reader) (io.ReadCloser, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("zstd decoder: %w", err)
	}
	return decoderCloser{dec}, nil
}

type decoderCloser struct {
	*zstd.Decoder
}

func (d decoderCloser) Close() error {
	d.Decoder.Close()
	return nil
}

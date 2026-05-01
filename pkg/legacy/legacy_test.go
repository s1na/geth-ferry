package legacy_test

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"

	"github.com/s1na/geth-ferry/pkg/backend/file"
	"github.com/s1na/geth-ferry/pkg/legacy"
)

func TestLegacyZstdRoundTrip(t *testing.T) {
	testLegacyRoundTrip(t, "archive-pbss-100-20260430.tar.zst", zstdEncode)
}

func TestLegacyLz4RoundTrip(t *testing.T) {
	testLegacyRoundTrip(t, "chaindata-100.tar.lz4", lz4Encode)
}

func TestLegacyUnknownExtension(t *testing.T) {
	tmp := t.TempDir()
	bucket := filepath.Join(tmp, "bucket")
	dst := filepath.Join(tmp, "dst")

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}
	w, err := be.Put(context.Background(), "snapshot.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("not a real snapshot"))
	w.Close()

	err = legacy.Download(context.Background(), be, "snapshot.tar.gz", legacy.Options{DataDir: dst})
	if err == nil {
		t.Fatalf("expected error for unknown extension, got nil")
	}
}

// testLegacyRoundTrip builds a fake legacy single-file snapshot using encode,
// uploads it via the file:// backend, downloads via legacy.Download, and
// asserts the extracted tree matches what was tarred.
func testLegacyRoundTrip(t *testing.T, key string, encode func(io.Writer) io.WriteCloser) {
	t.Helper()
	tmp := t.TempDir()
	bucket := filepath.Join(tmp, "bucket")
	dst := filepath.Join(tmp, "dst")

	// Build the tar contents in memory: a few chaindata-rooted files so the
	// extracted layout matches what geth expects.
	type entry struct {
		name string
		data []byte
	}
	entries := []entry{
		{"chaindata/CURRENT", []byte("MANIFEST-000001\n")},
		{"chaindata/MANIFEST-000001", randomBytes(t, 4096)},
		{"chaindata/000001.sst", randomBytes(t, 32*1024)},
		{"chaindata/ancient/chain/headers.0000.cdat", randomBytes(t, 8192)},
	}

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}
	w, err := be.Put(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	codecW := encode(w)
	tw := tar.NewWriter(codecW)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name,
			Mode: 0o644,
			Size: int64(len(e.data)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := codecW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if err := legacy.Download(context.Background(), be, key, legacy.Options{DataDir: dst}); err != nil {
		t.Fatalf("download: %v", err)
	}

	for _, e := range entries {
		got, err := os.ReadFile(filepath.Join(dst, "geth", filepath.FromSlash(e.name)))
		if err != nil {
			t.Errorf("read %s: %v", e.name, err)
			continue
		}
		if string(got) != string(e.data) {
			t.Errorf("file %s differs", e.name)
		}
	}
}

func zstdEncode(w io.Writer) io.WriteCloser {
	enc, err := zstd.NewWriter(w)
	if err != nil {
		panic(err)
	}
	return enc
}

func lz4Encode(w io.Writer) io.WriteCloser {
	return lz4.NewWriter(w)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

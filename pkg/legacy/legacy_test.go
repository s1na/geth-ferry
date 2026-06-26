package legacy_test

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"

	"github.com/s1na/geth-ferry/pkg/backend/file"
	"github.com/s1na/geth-ferry/pkg/legacy"
)

func TestLegacyZstdRoundTrip(t *testing.T) {
	testLegacyRoundTrip(t, "archive-pbss-100-20260430.tar.zst", zstdEncode, "chaindata/")
}

func TestLegacyLz4RoundTrip(t *testing.T) {
	testLegacyRoundTrip(t, "chaindata-100.tar.lz4", lz4Encode, "chaindata/")
}

// TestLegacyFlatTar covers the older benchmarker format where tar entries
// are NOT prefixed with "chaindata/": they're flat (e.g. "000016.sst",
// "ancient/chain/..."). Ferry must rebase those into <datadir>/geth/chaindata/.
func TestLegacyFlatTar(t *testing.T) {
	testLegacyRoundTrip(t, "chaindata-5000000.tar.lz4", lz4Encode, "")
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
// asserts the extracted tree matches what was tarred. tarPrefix is the path
// prefix to use on every tar entry name: "chaindata/" for the modern
// format, "" for the older benchmarker format. Either way, the
// downloaded files should end up at <dst>/geth/chaindata/...
func testLegacyRoundTrip(t *testing.T, key string, encode func(io.Writer) io.WriteCloser, tarPrefix string) {
	t.Helper()
	tmp := t.TempDir()
	bucket := filepath.Join(tmp, "bucket")
	dst := filepath.Join(tmp, "dst")

	type entry struct {
		name string
		data []byte
	}
	entries := []entry{
		{tarPrefix + "CURRENT", []byte("MANIFEST-000001\n")},
		{tarPrefix + "MANIFEST-000001", randomBytes(t, 4096)},
		{tarPrefix + "000001.sst", randomBytes(t, 32*1024)},
		{tarPrefix + "ancient/chain/headers.0000.cdat", randomBytes(t, 8192)},
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

	// Regardless of the input tar's prefix, we expect files to land under
	// <dst>/geth/chaindata/...
	for _, e := range entries {
		stripped := strings.TrimPrefix(e.name, tarPrefix)
		expected := filepath.Join(dst, "geth", "chaindata", filepath.FromSlash(stripped))
		got, err := os.ReadFile(expected)
		if err != nil {
			t.Errorf("read %s: %v", expected, err)
			continue
		}
		if string(got) != string(e.data) {
			t.Errorf("file %s differs", expected)
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

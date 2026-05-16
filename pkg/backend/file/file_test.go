package file

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s1na/geth-ferry/pkg/backend"
)

// TestPutCommit covers the happy path: written bytes survive Close.
func TestPutCommit(t *testing.T) {
	be, _ := newTestBackend(t)
	w, err := be.Put(context.Background(), "obj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := be.Get(context.Background(), "obj")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("Get returned %q, want %q", got, "hello")
	}
}

// TestPutAbort guarantees Abort leaves no visible object and no stray
// temp files behind. The latter check matters: a previous bug class is
// to leak ".ferry-*" temp files when the upload fails mid-stream.
func TestPutAbort(t *testing.T) {
	be, root := newTestBackend(t)
	w, err := be.Put(context.Background(), "obj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "garbage that should never land"); err != nil {
		t.Fatal(err)
	}
	w.Abort()

	if _, err := be.Get(context.Background(), "obj"); !errors.Is(err, backend.ErrNotExist) {
		t.Errorf("Get after Abort: err = %v, want ErrNotExist", err)
	}
	// Stray temp file check.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferry-") {
			t.Errorf("Abort left temp file %q in %s", e.Name(), root)
		}
	}
}

// TestPutAbortAfterCloseIsNoOp: termination is idempotent in either order.
func TestPutAbortAfterCloseIsNoOp(t *testing.T) {
	be, _ := newTestBackend(t)
	w, err := be.Put(context.Background(), "obj")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "ok")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Abort after Close must not destroy the committed object.
	w.Abort()
	r, err := be.Get(context.Background(), "obj")
	if err != nil {
		t.Fatalf("Get after Close+Abort: %v", err)
	}
	r.Close()
}

// TestGetNotExist confirms the sentinel is returned, wrapped, by Get.
func TestGetNotExist(t *testing.T) {
	be, _ := newTestBackend(t)
	_, err := be.Get(context.Background(), "missing")
	if !errors.Is(err, backend.ErrNotExist) {
		t.Errorf("Get(missing): err = %v, want errors.Is(err, ErrNotExist)", err)
	}
}

// TestStatNotExist confirms the same for Stat.
func TestStatNotExist(t *testing.T) {
	be, _ := newTestBackend(t)
	_, err := be.Stat(context.Background(), "missing")
	if !errors.Is(err, backend.ErrNotExist) {
		t.Errorf("Stat(missing): err = %v, want errors.Is(err, ErrNotExist)", err)
	}
}

func newTestBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bucket")
	be, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return be, root
}

var _ backend.Backend = (*Backend)(nil)

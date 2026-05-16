// Package file implements the Backend interface against the local filesystem.
// It is used for tests and for staging snapshots to a local mount.
//
// URLs of the form file:///abs/path map onto a backend rooted at /abs/path.
// All keys are interpreted relative to that root and joined with filepath.
package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/s1na/geth-ferry/pkg/backend"
)

// Backend is a local-filesystem implementation of backend.Backend rooted at Root.
type Backend struct {
	Root string
}

// New returns a Backend rooted at the given absolute directory. The directory
// is created if it doesn't exist.
func New(root string) (*Backend, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create root %s: %w", abs, err)
	}
	return &Backend{Root: abs}, nil
}

// FromURL constructs a file:// backend from a parsed URL.
func FromURL(u *url.URL) (*Backend, error) {
	if u.Scheme != "file" {
		return nil, fmt.Errorf("file backend: scheme %q unsupported", u.Scheme)
	}
	if u.Host != "" && u.Host != "localhost" {
		return nil, fmt.Errorf("file backend: host %q unsupported", u.Host)
	}
	path := u.Path
	if path == "" {
		return nil, fmt.Errorf("file backend: empty path in %s", u)
	}
	return New(path)
}

func (b *Backend) abs(key string) string {
	return filepath.Join(b.Root, filepath.FromSlash(key))
}

func (b *Backend) List(ctx context.Context, prefix string) ([]backend.Object, error) {
	root := b.abs(prefix)
	var out []backend.Object
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(b.Root, path)
		if err != nil {
			return err
		}
		out = append(out, backend.Object{
			Key:     filepath.ToSlash(rel),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

func (b *Backend) Stat(ctx context.Context, key string) (backend.Object, error) {
	info, err := os.Stat(b.abs(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return backend.Object{}, fmt.Errorf("stat %s: %w", key, backend.ErrNotExist)
		}
		return backend.Object{}, err
	}
	return backend.Object{
		Key:     key,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f, err := os.Open(b.abs(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("get %s: %w", key, backend.ErrNotExist)
		}
		return nil, err
	}
	return f, nil
}

// Put writes to a temp file alongside the destination, then atomically
// renames on Close. A writer that is Abort()'d (or never terminated) leaves
// no partial object visible.
func (b *Backend) Put(ctx context.Context, key string) (backend.Writer, error) {
	dst := b.abs(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ferry-"+filepath.Base(dst)+".*")
	if err != nil {
		return nil, err
	}
	return &atomicFile{f: tmp, finalPath: dst}, nil
}

func (b *Backend) Delete(ctx context.Context, key string) error {
	err := os.Remove(b.abs(key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// atomicFile buffers writes to a temp file next to the destination. Close
// renames into place; Abort closes and removes the temp file. Either method
// is idempotent; calling one disables the other.
type atomicFile struct {
	f         *os.File
	finalPath string
	done      bool
}

func (a *atomicFile) Write(p []byte) (int, error) {
	return a.f.Write(p)
}

func (a *atomicFile) Close() error {
	if a.done {
		return nil
	}
	a.done = true
	if err := a.f.Close(); err != nil {
		_ = os.Remove(a.f.Name())
		return err
	}
	if err := os.Rename(a.f.Name(), a.finalPath); err != nil {
		_ = os.Remove(a.f.Name())
		return err
	}
	return nil
}

func (a *atomicFile) Abort() {
	if a.done {
		return
	}
	a.done = true
	_ = a.f.Close()
	_ = os.Remove(a.f.Name())
}

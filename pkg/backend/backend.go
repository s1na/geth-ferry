// Package backend defines the storage abstraction ferry uses to read and write
// snapshots. Implementations live in subpackages and are dispatched by URL
// scheme via Open.
package backend

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotExist is returned (or wrapped) by Get and Stat when the requested
// key is absent. Callers should test with errors.Is rather than equality.
var ErrNotExist = errors.New("backend: object not found")

// Object describes a single stored object.
type Object struct {
	Key     string
	Size    int64
	ETag    string
	ModTime time.Time
}

// Backend is the abstract object store. A Backend instance is bound to a
// "root" — typically a bucket prefix or a local directory — and all keys
// passed to its methods are interpreted relative to that root.
type Backend interface {
	// List returns objects whose keys begin with the given relative prefix.
	List(ctx context.Context, prefix string) ([]Object, error)

	// Stat returns metadata for a single object. Returns ErrNotExist
	// (wrapped) when the key is absent.
	Stat(ctx context.Context, key string) (Object, error)

	// Get returns a streaming reader for the object's bytes. Returns
	// ErrNotExist (wrapped) when the key is absent. The caller must Close
	// the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Put returns a streaming Writer that uploads bytes to the given key.
	// The caller must terminate the writer by calling either Close
	// (commit) or Abort (discard) — never both, and exactly one. The
	// object is not visible to readers until Close returns nil.
	// Implementations may use multipart upload internally.
	Put(ctx context.Context, key string) (Writer, error)

	// Delete removes the object at key. Absent keys are not an error.
	Delete(ctx context.Context, key string) error
}

// Writer is the streaming-upload handle returned by Backend.Put. Exactly
// one of Close or Abort must be called to release backend resources:
//
//   - Close commits the object. After a successful Close, the object is
//     visible to subsequent Get/Stat calls.
//   - Abort discards any buffered or in-flight bytes; no object becomes
//     visible. Abort is best-effort and does not return an error — the
//     caller is already on an error path.
//
// Calling Close after Abort, or vice versa, is a no-op. Calling Write
// after either is undefined.
type Writer interface {
	io.Writer
	Close() error
	Abort()
}

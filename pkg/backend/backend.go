// Package backend defines the storage abstraction ferry uses to read and write
// snapshots. Implementations live in subpackages and are dispatched by URL
// scheme via Open.
package backend

import (
	"context"
	"io"
	"time"
)

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

	// Stat returns metadata for a single object. Returns an error wrapping
	// ErrNotExist when the key is absent.
	Stat(ctx context.Context, key string) (Object, error)

	// Get returns a streaming reader for the object's bytes. The caller must
	// Close it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Put returns a streaming writer that uploads bytes to the given key.
	// Close finalizes the upload; the object is not visible until Close
	// returns nil. Implementations may use multipart upload internally.
	Put(ctx context.Context, key string) (io.WriteCloser, error)

	// Delete removes the object at key. Absent keys are not an error.
	Delete(ctx context.Context, key string) error
}

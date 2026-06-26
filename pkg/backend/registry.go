package backend

import (
	"fmt"
	"net/url"
)

// OpenConfig carries per-Open tuning that some backends consume and others
// ignore. Fields default to the zero value, which each backend interprets
// as "use my default".
type OpenConfig struct {
	// MultipartPartSize is the size of each part of a multipart upload, in
	// bytes. Honored by s3; ignored by file.
	MultipartPartSize int

	// MultipartConcurrency is the number of UploadPart requests a single
	// Put may have in flight at once. Honored by s3; ignored by file.
	MultipartConcurrency int
}

// Option mutates an OpenConfig. Use the With* helpers below.
type Option func(*OpenConfig)

// WithMultipartPartSize sets the multipart part size, in bytes. Zero or
// negative is treated as "use the backend default".
func WithMultipartPartSize(n int) Option {
	return func(c *OpenConfig) { c.MultipartPartSize = n }
}

// WithMultipartConcurrency sets the maximum number of in-flight UploadPart
// requests per Put. Zero or negative is treated as "use the backend default".
func WithMultipartConcurrency(n int) Option {
	return func(c *OpenConfig) { c.MultipartConcurrency = n }
}

// Open returns a Backend rooted at the given URL. Backends are dispatched
// by the URL's scheme. Options unrecognized by the chosen backend are
// silently ignored; backend-specific options don't need to be guarded
// behind URL-scheme checks at the call site.
//
// Implementations register themselves via Register at init time so this
// package doesn't import each backend directly (and thus avoid pulling
// AWS SDK into tests that only need file://).
func Open(rawURL string, opts ...Option) (Backend, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse %q: %w", rawURL, err)
	}
	cfg := OpenConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	opener, ok := openers[u.Scheme]
	if !ok {
		return nil, "", fmt.Errorf("no backend registered for scheme %q", u.Scheme)
	}
	be, prefix, err := opener(u, &cfg)
	if err != nil {
		return nil, "", err
	}
	return be, prefix, nil
}

// Opener constructs a Backend from a parsed URL plus the OpenConfig, and
// returns the in-backend prefix that the caller's keys should hang off of
// (typically the URL path for object stores).
type Opener func(*url.URL, *OpenConfig) (Backend, string, error)

var openers = map[string]Opener{}

// Register installs an Opener for a URL scheme. Panics on duplicate
// registration; backends are expected to call this exactly once at init.
func Register(scheme string, o Opener) {
	if _, dup := openers[scheme]; dup {
		panic(fmt.Sprintf("backend: scheme %q already registered", scheme))
	}
	openers[scheme] = o
}

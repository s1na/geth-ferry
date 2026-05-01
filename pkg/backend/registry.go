package backend

import (
	"fmt"
	"net/url"
)

// Open returns a Backend rooted at the given URL. Backends are dispatched by
// the URL's scheme.
//
// Implementations register themselves via Register at init time so this
// package doesn't import each backend directly (and thus avoid pulling
// AWS SDK into tests that only need file://).
func Open(rawURL string) (Backend, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse %q: %w", rawURL, err)
	}
	opener, ok := openers[u.Scheme]
	if !ok {
		return nil, "", fmt.Errorf("no backend registered for scheme %q", u.Scheme)
	}
	be, prefix, err := opener(u)
	if err != nil {
		return nil, "", err
	}
	return be, prefix, nil
}

// Opener constructs a Backend from a parsed URL and returns the in-backend
// prefix that the caller's keys should hang off of (typically the URL path
// for object stores).
type Opener func(*url.URL) (Backend, string, error)

var openers = map[string]Opener{}

// Register installs an Opener for a URL scheme. Panics on duplicate
// registration; backends are expected to call this exactly once at init.
func Register(scheme string, o Opener) {
	if _, dup := openers[scheme]; dup {
		panic(fmt.Sprintf("backend: scheme %q already registered", scheme))
	}
	openers[scheme] = o
}

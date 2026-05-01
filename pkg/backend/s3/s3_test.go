package s3

import (
	"context"
	"crypto/rand"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/s1na/geth-ferry/pkg/backend"
)

func TestCanonPrefix(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"/":             "",
		"foo":           "foo/",
		"foo/":          "foo/",
		"/foo":          "foo/",
		"/foo/bar":      "foo/bar/",
		"/foo/bar/":     "foo/bar/",
		"snapshots/v1/": "snapshots/v1/",
	}
	for in, want := range cases {
		if got := canonPrefix(in); got != want {
			t.Errorf("canonPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"s3.gra.io.cloud.ovh.net":         "https://s3.gra.io.cloud.ovh.net",
		"https://s3.gra.io.cloud.ovh.net": "https://s3.gra.io.cloud.ovh.net",
		"http://localhost:9000":           "http://localhost:9000",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFromURLValidation covers the URL parsing without making any network
// calls — config loading is lazy so an invalid endpoint isn't reached
// until a request is made.
func TestFromURLValidation(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	t.Setenv("AWS_REGION", "us-east-1")

	cases := []struct {
		name   string
		raw    string
		wantOK bool
	}{
		{"basic", "s3://bucket/prefix/?endpoint=s3.example.com&region=eu", true},
		{"no host", "s3:///prefix", false},
		{"wrong scheme", "https://bucket/prefix", false},
		{"bad path_style", "s3://bucket/?path_style=maybe", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = FromURL(context.Background(), u)
			if (err == nil) != c.wantOK {
				t.Fatalf("FromURL err = %v, wantOK = %v", err, c.wantOK)
			}
		})
	}
}

// TestRoundTripIntegration exercises a real S3-compatible backend end-to-end
// when FERRY_S3_TEST_URL is set. It uploads, lists, downloads, deletes a
// small object and asserts equality. Skipped otherwise so `go test ./...`
// stays hermetic.
//
// Example:
//
//	export FERRY_S3_TEST_URL='s3://my-bucket/ferry-test/?endpoint=s3.amazonaws.com&region=us-east-1&path_style=false'
//	export AWS_ACCESS_KEY_ID=...
//	export AWS_SECRET_ACCESS_KEY=...
//	go test ./pkg/backend/s3/ -run TestRoundTripIntegration -v
func TestRoundTripIntegration(t *testing.T) {
	raw := os.Getenv("FERRY_S3_TEST_URL")
	if raw == "" {
		t.Skip("set FERRY_S3_TEST_URL to run S3 integration test")
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	be, _, err := FromURL(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	key := "ferry-roundtrip-" + time.Now().UTC().Format("20060102T150405.000")
	want := make([]byte, 1024*1024)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}

	// Put.
	w, err := be.Put(ctx, key)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := io.Copy(w, strings.NewReader(string(want))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Stat.
	obj, err := be.Stat(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if obj.Size != int64(len(want)) {
		t.Errorf("stat size = %d, want %d", obj.Size, len(want))
	}

	// List under a deliberately-narrow prefix.
	list, err := be.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) == 0 {
		t.Errorf("list returned no objects")
	}

	// Get.
	r, err := be.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("body differs (%d vs %d bytes)", len(got), len(want))
	}

	// Cleanup.
	if err := be.Delete(ctx, key); err != nil {
		t.Errorf("delete: %v", err)
	}
}

var _ backend.Backend = (*Backend)(nil)

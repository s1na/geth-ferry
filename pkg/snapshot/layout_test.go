package snapshot

import "testing"

func TestIsURL(t *testing.T) {
	cases := map[string]bool{
		"s3://bucket/prefix":     true,
		"file:///tmp/x":          true,
		"http://host/path":       true,
		"https://host/path":      true,
		"/local/path":            false,
		"relative/path":          false,
		"ftp://unsupported/host": false,
	}
	for in, want := range cases {
		if got := IsURL(in); got != want {
			t.Errorf("IsURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSplitTrailingSegment(t *testing.T) {
	cases := []struct {
		in       string
		wantRoot string
		wantName string
		wantErr  bool
	}{
		{"s3://bucket/snapshots/geth-1-archive-100-1746014400", "s3://bucket/snapshots", "geth-1-archive-100-1746014400", false},
		{"s3://bucket/snapshots/geth-1-archive-100-1746014400/", "s3://bucket/snapshots", "geth-1-archive-100-1746014400", false},
		{"s3://bucket/snapshots/foo.tar.zst?endpoint=x&region=y", "s3://bucket/snapshots?endpoint=x&region=y", "foo.tar.zst", false},
		{"file:///tmp/snapshots/foo", "file:///tmp/snapshots", "foo", false},
		{"s3://bucket/", "", "", true},
		{"s3://bucket", "", "", true},
	}
	for _, c := range cases {
		root, name, err := SplitTrailingSegment(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("SplitTrailingSegment(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if root != c.wantRoot || name != c.wantName {
			t.Errorf("SplitTrailingSegment(%q) = (%q, %q), want (%q, %q)",
				c.in, root, name, c.wantRoot, c.wantName)
		}
	}
}

func TestIsLegacyURL(t *testing.T) {
	cases := map[string]bool{
		// path-only inputs
		"chaindata-100.tar.lz4":     true,
		"/path/to/snapshot.tar.zst": true,
		"snapshot.tar":              false,
		"snapshot.tar.gz":           false,
		// real-world URLs with query parameters — these used to fail
		"s3://bucket/benchmarkers/chaindata-5000000.tar.lz4?endpoint=s3.de.io.cloud.ovh.net&region=de": true,
		"s3://bucket/snapshots/archive-1.tar.zst?region=de":                                            true,
		// snapshot directory shape (no .tar.* suffix)
		"s3://bucket/snapshots/geth-1-archive-100-1746014400":                  false,
		"s3://bucket/snapshots/geth-1-archive-100-1746014400?endpoint=foo.com": false,
		// uppercase suffix should still match
		"s3://bucket/SNAPSHOT.TAR.ZST?region=de": true,
	}
	for in, want := range cases {
		if got := IsLegacyURL(in); got != want {
			t.Errorf("IsLegacyURL(%q) = %v, want %v", in, got, want)
		}
	}
}

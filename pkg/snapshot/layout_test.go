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

func TestParseName(t *testing.T) {
	cases := []struct {
		in        string
		wantChain uint64
		wantRole  Role
		wantBlock uint64
		wantErr   bool
	}{
		// Canonical 4-part form.
		{"geth-1-archive-23456789", 1, RoleArchive, 23456789, false},
		{"geth-11155111-full-100", 11155111, RoleFull, 100, false},

		// Negative cases: wrong shape.
		{"geth-1-archive", 0, "", 0, true},                     // missing block
		{"geth-1-foo-100", 0, "", 0, true},                     // bad role
		{"geth-1-archive-100-x", 0, "", 0, true},               // trailing junk
		{"geth-1-archive-23456789-1746014400", 0, "", 0, true}, // legacy 5-part form is no longer parsed (≤ v0.1.0)
		{"geth-1-full-15000035-1778899789", 0, "", 0, true},    // ditto
		{"NOT-a-snapshot", 0, "", 0, true},
	}
	for _, c := range cases {
		got, err := ParseName(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseName(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if got.ChainID != c.wantChain || got.Role != c.wantRole ||
			got.Block != c.wantBlock {
			t.Errorf("ParseName(%q) = %+v, want chain=%d role=%q block=%d",
				c.in, got, c.wantChain, c.wantRole, c.wantBlock)
		}
	}
}

func TestNameString(t *testing.T) {
	in := Name{ChainID: 1, Role: RoleArchive, Block: 23456789}
	want := "geth-1-archive-23456789"
	if got := in.String(); got != want {
		t.Errorf("(%+v).String() = %q, want %q", in, got, want)
	}
}

func TestValidateNamePathSafety(t *testing.T) {
	ok := []string{
		// Canonical shape.
		"geth-1-archive-23456789",
		"geth-11155111-full-100",
		// Path-safety doesn't care about the canonical-name regex;
		// a legacy 5-part name is still a valid path segment.
		"geth-1-full-15000035-1778899789",
		// Free-form, path-safe: should now be accepted.
		"my-snapshot",
		"benchmarker-v2",
		"test_run_42",
		"latest.archive",
		"a", // single char ok
	}
	for _, n := range ok {
		if err := ValidateNamePathSafety(n); err != nil {
			t.Errorf("ValidateNamePathSafety(%q) rejected unexpectedly: %v", n, err)
		}
	}
	bad := []string{
		"",                    // empty
		".",                   // reserved
		"..",                  // reserved
		"foo/bar",             // slash → unintended sub-prefix
		"with space",          // whitespace
		"name?query=1",        // URL metachar
		"name#fragment",       // URL metachar
		"name\nwith\nnewline", // control char
	}
	for _, n := range bad {
		if err := ValidateNamePathSafety(n); err == nil {
			t.Errorf("ValidateNamePathSafety(%q) accepted, expected error", n)
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
		// real-world URLs with query parameters: these used to fail
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

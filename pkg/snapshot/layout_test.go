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
		wantTS    int64
		wantErr   bool
	}{
		// Current 4-part form.
		{"geth-1-archive-23456789", 1, RoleArchive, 23456789, 0, false},
		{"geth-11155111-full-100", 11155111, RoleFull, 100, 0, false},

		// Legacy 5-part form with a trailing unix-seconds tail.
		{"geth-1-archive-23456789-1746014400", 1, RoleArchive, 23456789, 1746014400, false},
		{"geth-1-full-15000035-1778899789", 1, RoleFull, 15000035, 1778899789, false},

		// Negative cases: wrong shape.
		{"geth-1-archive", 0, "", 0, 0, true},           // missing block
		{"geth-1-foo-100", 0, "", 0, 0, true},           // bad role
		{"geth-1-archive-100-12345", 0, "", 0, 0, true}, // 5-digit "timestamp" — too short to be plausible
		{"geth-1-archive-100-x", 0, "", 0, 0, true},     // non-numeric tail
		{"NOT-a-snapshot", 0, "", 0, 0, true},
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
			got.Block != c.wantBlock || got.Timestamp != c.wantTS {
			t.Errorf("ParseName(%q) = %+v, want chain=%d role=%q block=%d ts=%d",
				c.in, got, c.wantChain, c.wantRole, c.wantBlock, c.wantTS)
		}
	}
}

func TestNameString(t *testing.T) {
	cases := []struct {
		in   Name
		want string
	}{
		{Name{ChainID: 1, Role: RoleArchive, Block: 23456789}, "geth-1-archive-23456789"},
		{Name{ChainID: 1, Role: RoleFull, Block: 15000035, Timestamp: 1778899789}, "geth-1-full-15000035-1778899789"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("(%+v).String() = %q, want %q", c.in, got, c.want)
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

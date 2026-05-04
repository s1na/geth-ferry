package snapshot

import "testing"

func TestIsLegacyURL(t *testing.T) {
	cases := map[string]bool{
		// path-only inputs
		"chaindata-100.tar.lz4":                    true,
		"/path/to/snapshot.tar.zst":                true,
		"snapshot.tar":                             false,
		"snapshot.tar.gz":                          false,
		// real-world URLs with query parameters — these used to fail
		"s3://bucket/benchmarkers/chaindata-5000000.tar.lz4?endpoint=s3.de.io.cloud.ovh.net&region=de": true,
		"s3://bucket/snapshots/archive-1.tar.zst?region=de":                                           true,
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

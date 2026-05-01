package upload_test

import (
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/s1na/geth-ferry/pkg/backend/file"
	"github.com/s1na/geth-ferry/pkg/download"
	"github.com/s1na/geth-ferry/pkg/snapshot"
	"github.com/s1na/geth-ferry/pkg/upload"
)

// TestRoundTrip writes a fake datadir, uploads it through a file:// backend,
// downloads it back into a fresh datadir, and asserts byte-for-byte equality
// of the round-tripped tree.
func TestRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	srcDataDir := filepath.Join(tmp, "src")
	bucket := filepath.Join(tmp, "bucket")
	dstDataDir := filepath.Join(tmp, "dst")

	makeFakeDatadir(t, srcDataDir)

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	name := "geth-1-archive-100-20260430"
	m, err := upload.Run(ctx, be, "", upload.Options{
		DataDir: srcDataDir,
		Name:    name,
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	// Expect: chaindata-live + ancient-chain + ancient-state + triedb.
	if len(m.Parts) != 4 {
		t.Fatalf("want 4 parts, got %d: %+v", len(m.Parts), m.Parts)
	}
	wantKinds := map[snapshot.PartKind]bool{
		snapshot.PartChaindataLive: false,
		snapshot.PartAncientChain:  false,
		snapshot.PartAncientState:  false,
		snapshot.PartTriedb:        false,
	}
	for _, p := range m.Parts {
		if _, known := wantKinds[p.Kind]; !known {
			t.Errorf("unexpected part kind %q", p.Kind)
		}
		wantKinds[p.Kind] = true
	}
	for k, seen := range wantKinds {
		if !seen {
			t.Errorf("missing part kind %q", k)
		}
	}
	if m.StateScheme != snapshot.StateSchemePath {
		t.Errorf("state scheme: got %q, want path", m.StateScheme)
	}
	for _, p := range m.Parts {
		if p.SHA256 == "" {
			t.Errorf("part %s has empty sha256", p.Name)
		}
		if p.UncompressedSize == 0 {
			t.Errorf("part %s has zero uncompressed size", p.Name)
		}
	}

	if _, err := download.Run(ctx, be, "", download.Options{
		DataDir: dstDataDir,
		Name:    name,
	}); err != nil {
		t.Fatalf("download: %v", err)
	}

	assertTreesEqual(t,
		filepath.Join(srcDataDir, "geth"),
		filepath.Join(dstDataDir, "geth"),
	)
}

// TestRoundTripHBSS confirms an HBSS-style node (no triedb/, no ancient/state/)
// produces exactly two parts (live + ancient-chain) and round-trips cleanly.
func TestRoundTripHBSS(t *testing.T) {
	tmp := t.TempDir()
	srcDataDir := filepath.Join(tmp, "src")
	bucket := filepath.Join(tmp, "bucket")
	dstDataDir := filepath.Join(tmp, "dst")

	gethDir := filepath.Join(srcDataDir, "geth")
	mustMkdir(t, filepath.Join(gethDir, "chaindata"))
	mustMkdir(t, filepath.Join(gethDir, "chaindata", "ancient", "chain"))
	mustWrite(t, filepath.Join(gethDir, "chaindata", "MANIFEST-000001"), randomBytes(t, 1024))
	mustWrite(t, filepath.Join(gethDir, "chaindata", "000001.sst"), randomBytes(t, 4096))
	mustWrite(t, filepath.Join(gethDir, "chaindata", "ancient", "chain", "headers.0000.cdat"), randomBytes(t, 4*1024))

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	name := "geth-1-full-50-20260430"
	m, err := upload.Run(ctx, be, "", upload.Options{
		DataDir: srcDataDir,
		Name:    name,
		Role:    snapshot.RoleFull,
		Block:   50,
		ChainID: 1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(m.Parts) != 2 {
		t.Fatalf("want 2 parts (live + ancient-chain), got %d: %+v", len(m.Parts), m.Parts)
	}
	if m.StateScheme != snapshot.StateSchemeHash {
		t.Errorf("state scheme: got %q, want hash (no triedb/)", m.StateScheme)
	}

	if _, err := download.Run(ctx, be, "", download.Options{
		DataDir: dstDataDir,
		Name:    name,
	}); err != nil {
		t.Fatalf("download: %v", err)
	}
	assertTreesEqual(t,
		filepath.Join(srcDataDir, "geth"),
		filepath.Join(dstDataDir, "geth"),
	)
}

// TestUploadRefusesUnexpectedAncientEntry guarantees the validator fails
// fast when ancient/ contains anything besides chain/ and state/.
func TestUploadRefusesUnexpectedAncientEntry(t *testing.T) {
	tmp := t.TempDir()
	srcDataDir := filepath.Join(tmp, "src")
	bucket := filepath.Join(tmp, "bucket")

	makeFakeDatadir(t, srcDataDir)
	// Inject a rogue freezer namespace.
	mustMkdir(t, filepath.Join(srcDataDir, "geth", "chaindata", "ancient", "logs"))
	mustWrite(t, filepath.Join(srcDataDir, "geth", "chaindata", "ancient", "logs", "anything.cdat"), randomBytes(t, 1024))

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}
	_, err = upload.Run(context.Background(), be, "", upload.Options{
		DataDir: srcDataDir,
		Name:    "geth-1-archive-100-20260430",
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
		Force:   true,
	})
	if err == nil {
		t.Fatalf("expected upload to refuse unexpected ancient/ entry")
	}
}

// TestUploadRefusesLockedDatadir confirms preflight refuses to run when
// LOCK exists, and that --force overrides.
func TestUploadRefusesLockedDatadir(t *testing.T) {
	tmp := t.TempDir()
	srcDataDir := filepath.Join(tmp, "src")
	bucket := filepath.Join(tmp, "bucket")

	makeFakeDatadir(t, srcDataDir)
	mustWrite(t, filepath.Join(srcDataDir, "geth", "LOCK"), nil)

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opts := upload.Options{
		DataDir: srcDataDir,
		Name:    "geth-1-archive-100-20260430",
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
	}
	if _, err := upload.Run(ctx, be, "", opts); err == nil {
		t.Fatalf("expected upload to refuse locked datadir")
	}
	opts.Force = true
	if _, err := upload.Run(ctx, be, "", opts); err != nil {
		t.Fatalf("upload with --force: %v", err)
	}
}

func makeFakeDatadir(t *testing.T, root string) {
	t.Helper()
	geth := filepath.Join(root, "geth")
	mustMkdir(t, filepath.Join(geth, "chaindata"))
	mustMkdir(t, filepath.Join(geth, "chaindata", "ancient", "chain"))
	mustMkdir(t, filepath.Join(geth, "chaindata", "ancient", "state"))
	mustMkdir(t, filepath.Join(geth, "triedb"))

	mustWrite(t, filepath.Join(geth, "chaindata", "CURRENT"), []byte("MANIFEST-000001\n"))
	mustWrite(t, filepath.Join(geth, "chaindata", "MANIFEST-000001"), randomBytes(t, 4096))
	mustWrite(t, filepath.Join(geth, "chaindata", "000001.sst"), randomBytes(t, 32*1024))
	mustWrite(t, filepath.Join(geth, "chaindata", "000002.sst"), randomBytes(t, 16*1024))
	mustWrite(t, filepath.Join(geth, "chaindata", "ancient", "chain", "headers.0000.cdat"), randomBytes(t, 8*1024))
	mustWrite(t, filepath.Join(geth, "chaindata", "ancient", "chain", "headers.cidx"), randomBytes(t, 256))
	mustWrite(t, filepath.Join(geth, "chaindata", "ancient", "state", "account.data.0000.cdat"), randomBytes(t, 4*1024))
	mustWrite(t, filepath.Join(geth, "triedb", "merkle.journal"), randomBytes(t, 64*1024))
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// assertTreesEqual checks that two directory trees contain the same set of
// regular files with byte-identical contents. Directories are compared
// structurally; modes are not.
func assertTreesEqual(t *testing.T, a, b string) {
	t.Helper()
	want := walkRegular(t, a)
	got := walkRegular(t, b)
	if len(want) != len(got) {
		t.Fatalf("file count differs: want %d (%v), got %d (%v)",
			len(want), keys(want), len(got), keys(got))
	}
	for rel, wantData := range want {
		gotData, ok := got[rel]
		if !ok {
			t.Errorf("missing file: %s", rel)
			continue
		}
		if string(wantData) != string(gotData) {
			t.Errorf("file %s differs (%d vs %d bytes)", rel, len(wantData), len(gotData))
		}
	}
}

func walkRegular(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

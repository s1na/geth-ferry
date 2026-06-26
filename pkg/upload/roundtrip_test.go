package upload_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	name := "geth-1-archive-100-1746014400"
	m, _, err := upload.Run(ctx, be, "", upload.Options{
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

	if _, _, err := download.Run(ctx, be, "", download.Options{
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

// TestRoundTripParallel exercises the parallel-parts path in both upload
// and download. With four-way concurrency on a four-part snapshot, every
// part runs in its own goroutine; -race coverage proves the pipeline
// stays correct under that schedule.
func TestRoundTripParallel(t *testing.T) {
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
	name := "geth-1-archive-100-1746014400"
	if _, _, err := upload.Run(ctx, be, "", upload.Options{
		DataDir:       srcDataDir,
		Name:          name,
		Role:          snapshot.RoleArchive,
		Block:         100,
		ChainID:       1,
		ParallelParts: 4,
	}); err != nil {
		t.Fatalf("parallel upload: %v", err)
	}
	if _, _, err := download.Run(ctx, be, "", download.Options{
		DataDir:       dstDataDir,
		Name:          name,
		ParallelParts: 4,
	}); err != nil {
		t.Fatalf("parallel download: %v", err)
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
	name := "geth-1-full-50-1746014400"
	m, _, err := upload.Run(ctx, be, "", upload.Options{
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

	if _, _, err := download.Run(ctx, be, "", download.Options{
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
	_, _, err = upload.Run(context.Background(), be, "", upload.Options{
		DataDir: srcDataDir,
		Name:    "geth-1-archive-100-1746014400",
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
		Name:    "geth-1-archive-100-1746014400",
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
	}
	if _, _, err := upload.Run(ctx, be, "", opts); err == nil {
		t.Fatalf("expected upload to refuse locked datadir")
	}
	opts.Force = true
	if _, _, err := upload.Run(ctx, be, "", opts); err != nil {
		t.Fatalf("upload with --force: %v", err)
	}
}

// TestUploadAcceptsFreeFormName confirms that --name no longer has to
// match the canonical geth-<chain>-<role>-<block> shape. Operators are
// free to pick whatever fits their pipeline; only path-safety
// (no slashes, no URL metacharacters) is enforced.
func TestUploadAcceptsFreeFormName(t *testing.T) {
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
	name := "my-custom-snapshot-name" // intentionally NOT the canonical shape
	if _, _, err := upload.Run(ctx, be, "", upload.Options{
		DataDir: srcDataDir,
		Name:    name,
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
	}); err != nil {
		t.Fatalf("upload with custom name: %v", err)
	}
	// Round-trip: proves the name is usable end-to-end, not just at
	// upload-validation time.
	if _, _, err := download.Run(ctx, be, "", download.Options{
		DataDir: dstDataDir,
		Name:    name,
	}); err != nil {
		t.Fatalf("download with custom name: %v", err)
	}
	assertTreesEqual(t,
		filepath.Join(srcDataDir, "geth"),
		filepath.Join(dstDataDir, "geth"),
	)
}

// TestUploadRejectsUnsafeName confirms that names with path-traversal
// or URL-meta characters still get rejected; the relaxation isn't a
// blanket "anything goes".
func TestUploadRejectsUnsafeName(t *testing.T) {
	tmp := t.TempDir()
	srcDataDir := filepath.Join(tmp, "src")
	bucket := filepath.Join(tmp, "bucket")
	makeFakeDatadir(t, srcDataDir)

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"",             // empty
		"with/slash",   // would create unintended sub-prefix
		"with space",   // whitespace
		"with?query=1", // URL metachar
	} {
		_, _, err := upload.Run(context.Background(), be, "", upload.Options{
			DataDir: srcDataDir,
			Name:    name,
			Role:    snapshot.RoleArchive,
			Block:   100,
			ChainID: 1,
		})
		if err == nil {
			t.Errorf("upload accepted unsafe name %q, expected error", name)
		}
	}
}

// TestUploadRefusesExistingSnapshot exercises the overwrite-protection
// path: an upload to a name whose manifest.json already exists must fail
// by default, and pass with Overwrite=true. Guards against fat-fingered
// --name re-use silently replacing a published snapshot whose sha256
// downstream consumers may have pinned.
func TestUploadRefusesExistingSnapshot(t *testing.T) {
	tmp := t.TempDir()
	srcDataDir := filepath.Join(tmp, "src")
	bucket := filepath.Join(tmp, "bucket")

	makeFakeDatadir(t, srcDataDir)

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	name := "geth-1-archive-100-1746014400"
	opts := upload.Options{
		DataDir: srcDataDir,
		Name:    name,
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
	}

	// Seed: one successful upload.
	if _, _, err := upload.Run(ctx, be, "", opts); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	// Re-upload same name must refuse.
	_, _, err = upload.Run(ctx, be, "", opts)
	if err == nil {
		t.Fatalf("expected re-upload to refuse existing snapshot, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got: %v", err)
	}

	// Same upload with Overwrite=true must succeed.
	opts.Overwrite = true
	if _, _, err := upload.Run(ctx, be, "", opts); err != nil {
		t.Fatalf("re-upload with Overwrite=true: %v", err)
	}
}

// TestDownloadAtomicOnFailure proves that a download failure (here:
// tampered manifest sha256) leaves the destination datadir untouched
// rather than half-populated. Pre-atomic ferry would have extracted
// parts directly into <dst>/geth/ and bailed mid-stream.
func TestDownloadAtomicOnFailure(t *testing.T) {
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
	name := "geth-1-archive-100-1746014400"
	if _, _, err := upload.Run(ctx, be, "", upload.Options{
		DataDir: srcDataDir,
		Name:    name,
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Corrupt the manifest's first part sha256 so the download must fail.
	manifestPath := filepath.Join(bucket, name, snapshot.ManifestFilename)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := bytes.Replace(raw,
		[]byte(`"sha256": "`),
		[]byte(`"sha256": "ffffffff`),
		1,
	)
	if err := os.WriteFile(manifestPath, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}

	// A non-empty datadir to prove --force preserves the original on failure.
	mustWrite(t, filepath.Join(dstDataDir, "geth", "preexisting.txt"), []byte("keep me"))

	_, _, err = download.Run(ctx, be, "", download.Options{
		DataDir: dstDataDir,
		Name:    name,
		Force:   true,
	})
	if err == nil {
		t.Fatalf("expected download to fail on tampered sha256")
	}

	// The original geth/preexisting.txt must still be there.
	got, err := os.ReadFile(filepath.Join(dstDataDir, "geth", "preexisting.txt"))
	if err != nil {
		t.Fatalf("preexisting file lost after failed atomic download: %v", err)
	}
	if string(got) != "keep me" {
		t.Errorf("preexisting file changed: got %q", got)
	}

	// And there should be no leftover partial directory.
	entries, err := os.ReadDir(dstDataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferry-partial-") {
			t.Errorf("scratch directory %q not cleaned up after failure", e.Name())
		}
	}
}

// TestUploadAbortsPartialPart proves that a tar-side failure during upload
// does not leave a committed-but-corrupt part on the remote. We trigger
// the failure by deleting a file from the datadir mid-walk would be
// racy; simpler: validate the post-condition by checking that absent a
// manifest the bucket has no .tar.zst objects we'd mistake for a snapshot.
// Real coverage of the Abort path comes from file_test.go.
func TestUploadAbortsPartialPartViaUnexpectedAncient(t *testing.T) {
	tmp := t.TempDir()
	srcDataDir := filepath.Join(tmp, "src")
	bucket := filepath.Join(tmp, "bucket")

	makeFakeDatadir(t, srcDataDir)
	// Inject a rogue freezer namespace: same trigger as the
	// existing TestUploadRefusesUnexpectedAncientEntry but we additionally
	// check that *no* part object leaked into the bucket.
	mustMkdir(t, filepath.Join(srcDataDir, "geth", "chaindata", "ancient", "logs"))
	mustWrite(t, filepath.Join(srcDataDir, "geth", "chaindata", "ancient", "logs", "anything.cdat"), randomBytes(t, 1024))

	be, err := file.New(bucket)
	if err != nil {
		t.Fatal(err)
	}
	name := "geth-1-archive-100-1746014400"
	_, _, err = upload.Run(context.Background(), be, "", upload.Options{
		DataDir: srcDataDir,
		Name:    name,
		Role:    snapshot.RoleArchive,
		Block:   100,
		ChainID: 1,
		Force:   true,
	})
	if err == nil {
		t.Fatalf("upload should have refused unexpected ancient/ entry")
	}
	// Validate no .tar.zst left under <bucket>/<name>/.
	snapDir := filepath.Join(bucket, name)
	if entries, statErr := os.ReadDir(snapDir); statErr == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".tar.zst") {
				t.Errorf("aborted upload left committed part %q", e.Name())
			}
		}
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

package snapshot

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Untar extracts the tar stream from r into dst. Entry names in the stream
// are interpreted relative to dst (so an entry "chaindata/foo" becomes
// <dst>/chaindata/foo). Entries that resolve outside dst are rejected.
//
// Only directories, regular files, and relative symlinks are honored;
// device files / fifos / hard links are skipped.
func Untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := safeExtract(tr, hdr, dst); err != nil {
			return err
		}
	}
}

// EnsureEmpty refuses to proceed if dir exists and is non-empty unless force
// is set. A missing dir is fine — the caller will create it. Used as the
// pre-extract guard for both manifest-based and legacy downloads.
func EnsureEmpty(dir string, force bool) error {
	if force {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty; pass --force to override", dir)
	}
	return nil
}

func safeExtract(tr *tar.Reader, hdr *tar.Header, dst string) error {
	clean := filepath.Clean(filepath.FromSlash(hdr.Name))
	if strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return fmt.Errorf("entry %q escapes destination", hdr.Name)
	}
	target := filepath.Join(dst, clean)
	if rel, err := filepath.Rel(dst, target); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("entry %q escapes destination", hdr.Name)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, hdr.FileInfo().Mode().Perm())
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdr.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, tr)
		if cerr := f.Close(); cerr != nil && copyErr == nil {
			copyErr = cerr
		}
		return copyErr
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if filepath.IsAbs(hdr.Linkname) {
			return fmt.Errorf("entry %q: absolute symlink target rejected", hdr.Name)
		}
		return os.Symlink(hdr.Linkname, target)
	default:
		return nil
	}
}

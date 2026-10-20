package registry

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
	maxSymlinks    = 255
)

// applyBlob downloads a layer to a temp file, verifies its sha256 digest, and
// only then extracts it into dest. Nothing from a corrupted or tampered blob
// ever touches the rootfs.
func applyBlob(body io.Reader, digest, dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "layer-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), body); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("digest mismatch: got %s want %s", got, digest)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return applyLayer(tmp, dest)
}

// applyLayer extracts a (optionally gzipped) layer tarball on top of root.
func applyLayer(r io.Reader, root string) error {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		src = gz
	}

	// Paths written by this layer, so an opaque whiteout only hides what
	// came from lower layers.
	inLayer := map[string]bool{}

	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := applyTarEntry(root, hdr, tr, inLayer); err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
}

func applyTarEntry(root string, hdr *tar.Header, r io.Reader, inLayer map[string]bool) error {
	name := path.Clean("/" + hdr.Name)[1:]
	if name == "" {
		return nil
	}
	base := path.Base(name)

	// Resolve the parent directory inside root. Symlinks created by earlier
	// entries are followed as if root were "/", so a layer can't use them
	// to write outside the container's rootfs.
	dir, err := secureJoin(root, path.Dir(name))
	if err != nil {
		return err
	}
	full := filepath.Join(dir, base)

	// The opaque marker also starts with ".wh.", so it must be checked first.
	if base == opaqueWhiteout {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if !inLayer[p] {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if strings.HasPrefix(base, whiteoutPrefix) {
		return os.RemoveAll(filepath.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)))
	}

	inLayer[full] = true
	mode := hdr.FileInfo().Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)

	switch hdr.Typeflag {
	case tar.TypeDir:
		if fi, err := os.Lstat(full); err == nil && !fi.IsDir() {
			if err := os.RemoveAll(full); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(full, 0o755); err != nil {
			return err
		}
		return os.Chmod(full, mode)

	case tar.TypeReg, tar.TypeRegA:
		if err := replace(dir, full); err != nil {
			return err
		}
		// O_EXCL|O_NOFOLLOW: never write through a pre-existing symlink.
		f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		if err := f.Chmod(mode); err != nil {
			f.Close()
			return err
		}
		return f.Close()

	case tar.TypeSymlink:
		if err := replace(dir, full); err != nil {
			return err
		}
		// The target is stored as-is; it's only ever resolved inside the
		// container (after pivot_root) or through secureJoin.
		return os.Symlink(hdr.Linkname, full)

	case tar.TypeLink:
		ln := path.Clean("/" + hdr.Linkname)[1:]
		tdir, err := secureJoin(root, path.Dir(ln))
		if err != nil {
			return err
		}
		if err := replace(dir, full); err != nil {
			return err
		}
		return os.Link(filepath.Join(tdir, path.Base(ln)), full)

	default:
		// Device nodes and FIFOs can't be created without real root; skip.
		return nil
	}
}

// replace makes sure dir exists and nothing is left at full.
func replace(dir, full string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.RemoveAll(full)
}

// secureJoin resolves unsafePath inside root, following any symlinks as if
// root were the filesystem root: absolute targets restart at root and ".."
// can't climb above it. The result is always within root.
func secureJoin(root, unsafePath string) (string, error) {
	resolved := "" // relative to root, no leading slash
	remaining := unsafePath
	links := 0

	for remaining != "" {
		var part string
		if i := strings.IndexByte(remaining, '/'); i >= 0 {
			part, remaining = remaining[:i], remaining[i+1:]
		} else {
			part, remaining = remaining, ""
		}

		switch part {
		case "", ".":
			continue
		case "..":
			resolved = path.Dir(resolved)
			if resolved == "." {
				resolved = ""
			}
			continue
		}

		next := path.Join(resolved, part)
		fi, err := os.Lstat(filepath.Join(root, next))
		if errors.Is(err, os.ErrNotExist) || (err == nil && fi.Mode()&os.ModeSymlink == 0) {
			resolved = next
			continue
		}
		if err != nil {
			return "", err
		}

		links++
		if links > maxSymlinks {
			return "", fmt.Errorf("too many symlinks resolving %q", unsafePath)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			resolved = ""
		}
		remaining = target + "/" + remaining
	}
	return filepath.Join(root, resolved), nil
}

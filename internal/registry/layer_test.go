package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
}

func dir(name string) entry             { return entry{name: name, typ: tar.TypeDir} }
func file(name, body string) entry      { return entry{name: name, body: body, typ: tar.TypeReg} }
func symlink(name, target string) entry { return entry{name: name, link: target, typ: tar.TypeSymlink} }
func hardlink(name, target string) entry {
	return entry{name: name, link: target, typ: tar.TypeLink}
}

func makeLayer(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func apply(t *testing.T, root string, entries ...entry) {
	t.Helper()
	if err := applyLayer(bytes.NewReader(makeLayer(t, entries...)), root); err != nil {
		t.Fatal(err)
	}
}

func readDir(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func assertMissing(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("%s should not exist (err=%v)", p, err)
	}
}

func assertContent(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", p, got, want)
	}
}

func TestWhiteoutRemovesLowerFile(t *testing.T) {
	root := t.TempDir()
	apply(t, root, dir("etc/"), file("etc/a", "a"), file("etc/b", "b"))
	apply(t, root, file("etc/.wh.a", ""))

	assertMissing(t, filepath.Join(root, "etc/a"))
	assertContent(t, filepath.Join(root, "etc/b"), "b")
}

func TestOpaqueWhiteoutHidesLowerLayers(t *testing.T) {
	root := t.TempDir()
	apply(t, root, dir("app/"), file("app/old1", "x"), file("app/old2", "y"))
	// Upper layer replaces the whole directory. Entries from the same layer
	// must survive regardless of where the marker appears.
	apply(t, root, dir("app/"), file("app/new", "n"), file("app/.wh..wh..opq", ""))

	got := readDir(t, filepath.Join(root, "app"))
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("app/ = %v, want [new]", got)
	}
	// The marker itself must not be extracted as a file.
	assertMissing(t, filepath.Join(root, "app", opaqueWhiteout))
}

func TestSymlinkCannotRedirectWritesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	apply(t, root,
		symlink("abs", outside),               // absolute host path
		symlink("rel", "../../../.."+outside), // climbs out with ..
		file("abs/pwned", "evil"),
		file("rel/pwned2", "evil"),
	)

	if got := readDir(t, outside); len(got) != 0 {
		t.Fatalf("files escaped root into %s: %v", outside, got)
	}
	// Both resolve inside root, as they would after pivot_root.
	assertContent(t, filepath.Join(root, outside, "pwned"), "evil")
	assertContent(t, filepath.Join(root, outside, "pwned2"), "evil")
}

func TestFileReplacesSymlinkInsteadOfWritingThroughIt(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "passwd")
	if err := os.WriteFile(victim, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}

	apply(t, root, dir("etc/"), symlink("etc/passwd", victim))
	apply(t, root, file("etc/passwd", "container"))

	assertContent(t, victim, "host")
	fi, err := os.Lstat(filepath.Join(root, "etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("etc/passwd mode = %v, want regular file", fi.Mode())
	}
}

func TestHardlinkTargetConfinedToRoot(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The target resolves to root/<secret path>, which doesn't exist, so the
	// link fails instead of exposing the host file inside the container.
	leak := makeLayer(t, hardlink("leak", "../../../.."+secret))
	if err := applyLayer(bytes.NewReader(leak), root); err == nil {
		t.Fatal("expected error linking to a target outside the layer")
	}
	assertMissing(t, filepath.Join(root, "leak"))

	apply(t, root, file("real", "data"), hardlink("same", "../real"))
	assertContent(t, filepath.Join(root, "same"), "data")
}

func TestApplyBlobVerifiesDigestBeforeExtracting(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "rootfs")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := makeLayer(t, file("hello", "world"))

	bad := "sha256:" + hex.EncodeToString(make([]byte, 32))
	if err := applyBlob(bytes.NewReader(blob), bad, root); err == nil {
		t.Fatal("expected digest mismatch error")
	}
	if got := readDir(t, root); len(got) != 0 {
		t.Fatalf("rootfs modified despite bad digest: %v", got)
	}
	if got := readDir(t, base); len(got) != 1 {
		t.Fatalf("temp file left behind: %v", got)
	}

	sum := sha256.Sum256(blob)
	if err := applyBlob(bytes.NewReader(blob), "sha256:"+hex.EncodeToString(sum[:]), root); err != nil {
		t.Fatal(err)
	}
	assertContent(t, filepath.Join(root, "hello"), "world")
}

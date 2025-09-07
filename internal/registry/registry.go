package registry

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type ImageRef struct {
	Registry string // e.g. registry-1.docker.io
	Repo     string // e.g. library/alpine
	Tag      string // e.g. latest
}

func (r ImageRef) String() string   { return r.Repo + ":" + r.Tag }
func (r ImageRef) RepoPath() string { return r.Repo }

func ParseImageRef(s string) (ImageRef, error) {
	// defaults: docker.io/library/<name>:latest
	tag := "latest"
	name := s
	if i := strings.LastIndexByte(s, ':'); i > 0 && !strings.Contains(s[i+1:], "/") {
		name = s[:i]
		tag = s[i+1:]
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return ImageRef{Registry: "registry-1.docker.io", Repo: name, Tag: tag}, nil
}

// Docker schema2 manifest
type Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Layers []Layer `json:"layers"`
}

type Layer struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
}

// Pull orchestrates auth -> manifest -> layers -> rootfs extract and saves config.json
func Pull(ref ImageRef, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	token, err := getToken(ref)
	if err != nil {
		return err
	}

	mani, rawConfig, err := getManifestAndConfig(ref, token)
	if err != nil {
		return err
	}

	rootfsDir := filepath.Join(dest, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return err
	}

	// download + apply layers in order
	for i, l := range mani.Layers {
		if err := fetchAndApplyLayer(ref, token, l.Digest, rootfsDir); err != nil {
			return fmt.Errorf("layer %d %s: %w", i, l.Digest, err)
		}
	}

	// save config JSON for Step 8
	if err := os.WriteFile(filepath.Join(dest, "config.json"), rawConfig, 0o644); err != nil {
		return err
	}

	return nil
}

func getToken(ref ImageRef) (string, error) {
	v := url.Values{}
	v.Set("service", "registry.docker.io")
	v.Set("scope", "repository:"+ref.Repo+":pull")
	u := url.URL{Scheme: "https", Host: "auth.docker.io", Path: "/token", RawQuery: v.Encode()}
	resp, err := http.Get(u.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("auth: %s", resp.Status)
	}
	var tmp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tmp); err != nil {
		return "", err
	}
	if tmp.Token == "" {
		return "", errors.New("empty token")
	}
	return tmp.Token, nil
}

func getManifestAndConfig(ref ImageRef, token string) (*Manifest, []byte, error) {
	req, _ := http.NewRequest("GET", "https://"+ref.Registry+"/v2/"+ref.Repo+"/manifests/"+ref.Tag, nil)
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("manifest: %s", resp.Status)
	}
	var mani Manifest
	if err := json.NewDecoder(resp.Body).Decode(&mani); err != nil {
		return nil, nil, err
	}

	// fetch config blob
	cfg, err := fetchBlob(ref, token, mani.Config.Digest)
	if err != nil {
		return nil, nil, err
	}
	return &mani, cfg, nil
}

func fetchBlob(ref ImageRef, token, digest string) ([]byte, error) {
	req, _ := http.NewRequest("GET", "https://"+ref.Registry+"/v2/"+ref.Repo+"/blobs/"+digest, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("blob %s: %s", digest, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func fetchAndApplyLayer(ref ImageRef, token, digest, dest string) error {
	// stream blob and verify sha256
	req, _ := http.NewRequest("GET", "https://"+ref.Registry+"/v2/"+ref.Repo+"/blobs/"+digest, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("blob %s: %s", digest, resp.Status)
	}

	// verify digest while extracting
	h := sha256.New()
	tee := io.TeeReader(resp.Body, h)

	// layers are gzip'ed tars
	gz, err := gzip.NewReader(tee)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := applyTarEntry(dest, hdr, tr); err != nil {
			return err
		}
	}

	sum := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if sum != digest {
		return fmt.Errorf("digest mismatch: got %s want %s", sum, digest)
	}
	return nil
}

func applyTarEntry(root string, hdr *tar.Header, r io.Reader) error {
	// normalize/secure path
	name := path.Clean("/" + hdr.Name)[1:] // strip leading slash after clean
	full := filepath.Join(root, name)

	base := path.Base(name)
	dir := filepath.Dir(full)

	// whiteouts
	if strings.HasPrefix(base, ".wh.") {
		target := filepath.Join(root, path.Dir(name), strings.TrimPrefix(base, ".wh."))
		return os.RemoveAll(target)
	}
	if base == ".wh..wh..opq" {
		// remove all existing entries under this directory (opaque)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
		return nil
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(full, os.FileMode(hdr.Mode))
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	case tar.TypeSymlink:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		_ = os.RemoveAll(full)
		return os.Symlink(hdr.Linkname, full)
	case tar.TypeLink:
		// hardlink inside layer
		target := filepath.Join(root, hdr.Linkname)
		_ = os.RemoveAll(full)
		return os.Link(target, full)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// skip device files/fifos in this challenge
		return nil
	default:
		return nil
	}
}

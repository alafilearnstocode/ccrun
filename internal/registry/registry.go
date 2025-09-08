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

// http client that preserves Authorization header across redirects
func authClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 {
				// copy all headers from the previous request; Docker Hub redirects to
				// a different host (e.g., cloud storage) and Go drops Authorization by default.
				for k, vv := range via[0].Header {
					req.Header[k] = vv
				}
			}
			return nil
		},
	}
}

func doGET(u string, hdr map[string]string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", u, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return authClient().Do(req)
}

func normalizeDigest(d string) string {
	if strings.HasPrefix(d, "sha256:") {
		return d
	}
	if len(d) == 64 {
		return "sha256:" + d
	}
	return d
}

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
// and manifest list (index)

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

type ManifestList struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Manifests     []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
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
		if err := fetchAndApplyLayer(ref, token, normalizeDigest(l.Digest), rootfsDir); err != nil {
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
	// Allow both manifest list and image manifest
	req, _ := http.NewRequest("GET", "https://"+ref.Registry+"/v2/"+ref.Repo+"/manifests/"+ref.Tag, nil)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := authClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("manifest: %s", resp.Status)
	}

	ct := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	if strings.Contains(ct, "manifest.list.v2+json") {
		var ml ManifestList
		if err := json.Unmarshal(body, &ml); err != nil {
			return nil, nil, err
		}
		pick := ""
		for _, m := range ml.Manifests { // prefer linux/arm64
			if m.Platform.OS == "linux" && m.Platform.Architecture == "arm64" {
				pick = m.Digest
				break
			}
		}
		if pick == "" {
			for _, m := range ml.Manifests { // fallback linux/amd64
				if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
					pick = m.Digest
					break
				}
			}
		}
		if pick == "" {
			return nil, nil, fmt.Errorf("no suitable platform in manifest list")
		}

		req2, _ := http.NewRequest("GET", "https://"+ref.Registry+"/v2/"+ref.Repo+"/manifests/"+pick, nil)
		req2.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
		req2.Header.Set("Authorization", "Bearer "+token)
		resp2, err := authClient().Do(req2)
		if err != nil {
			return nil, nil, err
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != 200 {
			return nil, nil, fmt.Errorf("manifest (platform): %s", resp2.Status)
		}
		var mani Manifest
		if err := json.NewDecoder(resp2.Body).Decode(&mani); err != nil {
			return nil, nil, err
		}
		cfg, err := fetchBlob(ref, token, normalizeDigest(mani.Config.Digest))
		if err != nil {
			return nil, nil, err
		}
		return &mani, cfg, nil
	}

	var mani Manifest
	if err := json.Unmarshal(body, &mani); err != nil {
		return nil, nil, err
	}
	cfg, err := fetchBlob(ref, token, normalizeDigest(mani.Config.Digest))
	if err != nil {
		return nil, nil, err
	}
	return &mani, cfg, nil
}

func fetchBlob(ref ImageRef, token, digest string) ([]byte, error) {
	digest = normalizeDigest(digest)
	u := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + digest
	resp, err := doGET(u, map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/octet-stream",
	})
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
	digest = normalizeDigest(digest)
	u := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + digest
	resp, err := doGET(u, map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/octet-stream",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("blob %s: %s", digest, resp.Status)
	}

	h := sha256.New()
	tee := io.TeeReader(resp.Body, h)

	// Some layers are gzip'd tars; some may be plain tar. Try gzip first.
	var tr *tar.Reader
	if gz, err := gzip.NewReader(tee); err == nil {
		defer gz.Close()
		tr = tar.NewReader(gz)
	} else {
		tr = tar.NewReader(tee)
	}

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

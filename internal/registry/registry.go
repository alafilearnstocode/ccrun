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
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// --- auth helpers (challenge flow) ---

type authParams struct {
	realm   string
	service string
	scope   string
}

func parseAuthHeader(h string) (authParams, bool) {
	// expected: Bearer realm="...",service="...",scope="..."
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return authParams{}, false
	}
	h = strings.TrimSpace(h[len(prefix):])
	parts := strings.Split(h, ",")
	ap := authParams{}
	for _, p := range parts {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(kv[0])
		v := strings.Trim(kv[1], "\"")
		switch k {
		case "realm":
			ap.realm = v
		case "service":
			ap.service = v
		case "scope":
			ap.scope = v
		}
	}
	if ap.realm == "" {
		return authParams{}, false
	}
	return ap, true
}

func fetchToken(ap authParams, repo string) (string, error) {
	// ensure scope
	scope := ap.scope
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q := url.Values{}
	q.Set("service", ap.service)
	q.Set("scope", scope)
	u := ap.realm + "?" + q.Encode()
	resp, err := http.Get(u)
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

// doGET issues a GET and, on 401 with a Bearer challenge, obtains a token and retries once.
func doGET(repo, urlStr, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "ccrun/0.1 (+https://github.com/alafilearnstocode/ccrun)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	// 401 -> parse challenge
	authH := resp.Header.Get("Www-Authenticate")
	resp.Body.Close()
	ap, ok := parseAuthHeader(authH)
	if !ok {
		return nil, fmt.Errorf("unauthorized and no Bearer challenge")
	}
	tok, err := fetchToken(ap, repo)
	if err != nil {
		return nil, err
	}
	// retry with token
	req2, _ := http.NewRequest("GET", urlStr, nil)
	if accept != "" {
		req2.Header.Set("Accept", accept)
	}
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("User-Agent", "ccrun/0.1 (+https://github.com/alafilearnstocode/ccrun)")
	return http.DefaultClient.Do(req2)
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

func normalizeDigest(d string) string { return strings.TrimSpace(d) }

func getBlob(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("blob: %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func fetchBlobWithFallback(ref ImageRef, token, digest string) ([]byte, error) {
	d := normalizeDigest(digest)
	// 1) try with algorithm prefix (sha256:<hex>)
	u := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + d
	log.Printf("blob try: %s", u)
	resp, err := doGET(ref.Repo, u, "application/octet-stream")
	if err == nil && resp.StatusCode == 200 {
		return getBlob(resp)
	}
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("blob GET %s -> %s, body=%q", u, resp.Status, string(b))
	}
	// 2) fallback: try without algorithm prefix if it exists
	if i := strings.IndexByte(d, ':'); i > 0 {
		alt := d[i+1:]
		u2 := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + alt
		log.Printf("blob try: %s", u2)
		resp2, err := doGET(ref.Repo, u2, "application/octet-stream")
		if err == nil && resp2.StatusCode == 200 {
			return getBlob(resp2)
		}
		if err == nil {
			b2, _ := io.ReadAll(resp2.Body)
			defer resp2.Body.Close()
			return nil, fmt.Errorf("blob: %s, body=%q", resp2.Status, string(b2))
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("blob: %s", resp.Status)
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

	mani, rawConfig, err := getManifestAndConfig(ref, "")
	if err != nil {
		return err
	}

	rootfsDir := filepath.Join(dest, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return err
	}

	// download + apply layers in order
	for i, l := range mani.Layers {
		if err := fetchAndApplyLayer(ref, "", l.Digest, rootfsDir); err != nil {
			return fmt.Errorf("layer %d %s: %w", i, l.Digest, err)
		}
	}

	// save config JSON for Step 8
	if err := os.WriteFile(filepath.Join(dest, "config.json"), rawConfig, 0o644); err != nil {
		return err
	}

	return nil
}

func getManifestAndConfig(ref ImageRef, token string) (*Manifest, []byte, error) {
	// Allow both manifest list (index) and image manifest
	acceptHeader := strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
	}, ", ")
	resp, err := doGET(ref.Repo, "https://"+ref.Registry+"/v2/"+ref.Repo+"/manifests/"+ref.Tag, acceptHeader)
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
				log.Printf("picked platform=%s/%s variant=%s digest=%s", m.Platform.OS, m.Platform.Architecture, m.Platform.Variant, m.Digest)
				break
			}
		}
		if pick == "" {
			for _, m := range ml.Manifests { // fallback linux/amd64
				if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
					pick = m.Digest
					log.Printf("picked platform=%s/%s variant=%s digest=%s", m.Platform.OS, m.Platform.Architecture, m.Platform.Variant, m.Digest)
					break
				}
			}
		}
		if pick == "" {
			return nil, nil, fmt.Errorf("no suitable platform in manifest list")
		}

		// Fetch selected image manifest
		resp2, err := doGET(ref.Repo, "https://"+ref.Registry+"/v2/"+ref.Repo+"/manifests/"+pick, "application/vnd.docker.distribution.manifest.v2+json")
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
		log.Printf("config digest: %s", mani.Config.Digest)
		cfg, err := fetchBlob(ref, "", mani.Config.Digest)
		if err != nil {
			return nil, nil, err
		}
		return &mani, cfg, nil
	}

	// Single image manifest
	var mani Manifest
	if err := json.Unmarshal(body, &mani); err != nil {
		return nil, nil, err
	}
	log.Printf("config digest: %s", mani.Config.Digest)
	cfg, err := fetchBlob(ref, "", mani.Config.Digest)
	if err != nil {
		return nil, nil, err
	}
	return &mani, cfg, nil
}

func fetchBlob(ref ImageRef, token, digest string) ([]byte, error) {
	return fetchBlobWithFallback(ref, token, digest)
}

func fetchAndApplyLayer(ref ImageRef, token, digest, dest string) error {
	d := normalizeDigest(digest)
	// try with algorithm prefix
	u := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + d
	log.Printf("layer try: %s", u)
	resp, err := doGET(ref.Repo, u, "application/octet-stream")
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("layer GET %s -> %s, body=%q", u, resp.Status, string(b))
		resp.Body.Close()
		if i := strings.IndexByte(d, ':'); i > 0 { // fallback without algo
			alt := d[i+1:]
			u2 := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + alt
			log.Printf("layer try: %s", u2)
			resp2, err := doGET(ref.Repo, u2, "application/octet-stream")
			if err != nil {
				return err
			}
			resp = resp2
			if resp.StatusCode != 200 {
				b2, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("blob %s: %s, body=%q", digest, resp.Status, string(b2))
			}
		}
	}

	defer resp.Body.Close()
	// verify digest while extracting
	h := sha256.New()
	tee := io.TeeReader(resp.Body, h)

	// Some layers are gzip'd; others may be plain tar. Try gzip first.
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

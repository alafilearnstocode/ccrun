package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func dbg(format string, args ...any) {
	if os.Getenv("CCRUN_HTTP_DEBUG") == "1" {
		fmt.Printf("[ccrun] "+format+"\n", args...)
	}
}

func authClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 {
				prev := via[0]

				if v := prev.Header.Get("Accept"); v != "" {
					req.Header.Set("Accept", v)
				}

				if v := prev.Header.Get("User-Agent"); v != "" {
					req.Header.Set("User-Agent", v)
				}

				if prev.URL.Host == req.URL.Host {
					if v := prev.Header.Get("Authorization"); v != "" {
						req.Header.Set("Authorization", v)
					}
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

	for i, l := range mani.Layers {
		if err := fetchAndApplyLayer(ref, token, normalizeDigest(l.Digest), rootfsDir); err != nil {
			return fmt.Errorf("layer %d %s: %w", i, l.Digest, err)
		}
	}

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
	dbg("auth request: %s (scope=%q)", u.String(), v.Get("scope"))
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
	if len(tmp.Token) > 16 {
		dbg("auth OK: token=%s...", tmp.Token[:16])
	} else {
		dbg("auth OK: token(len=%d)", len(tmp.Token))
	}
	if tmp.Token == "" {
		return "", errors.New("empty token")
	}
	return tmp.Token, nil
}

func getManifestAndConfig(ref ImageRef, token string) (*Manifest, []byte, error) {

	req, _ := http.NewRequest("GET", "https://"+ref.Registry+"/v2/"+ref.Repo+"/manifests/"+ref.Tag, nil)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := authClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	dbg("manifest GET %s -> %s", req.URL.String(), resp.Status)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("manifest: %s", resp.Status)
	}

	ct := resp.Header.Get("Content-Type")
	dbg("manifest content-type: %s", ct)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	if strings.Contains(ct, "manifest.list.v2+json") || strings.Contains(ct, "image.index.v1+json") {
		var ml ManifestList
		if err := json.Unmarshal(body, &ml); err != nil {
			return nil, nil, err
		}
		pick := ""
		for _, m := range ml.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == runtime.GOARCH {
				pick = m.Digest
				break
			}
		}
		if pick == "" {
			return nil, nil, fmt.Errorf("no linux/%s image in manifest list", runtime.GOARCH)
		}
		dbg("selected platform manifest digest: %s", pick)

		req2, _ := http.NewRequest("GET", "https://"+ref.Registry+"/v2/"+ref.Repo+"/manifests/"+pick, nil)
		req2.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json")
		req2.Header.Set("Authorization", "Bearer "+token)
		resp2, err := authClient().Do(req2)
		if err != nil {
			return nil, nil, err
		}
		dbg("platform manifest GET %s -> %s", req2.URL.String(), resp2.Status)
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

	if strings.Contains(ct, "manifest.v2+json") || strings.Contains(ct, "image.manifest.v1+json") {
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

	return nil, nil, fmt.Errorf("unsupported manifest content-type: %s", ct)
}

func fetchBlob(ref ImageRef, token, digest string) ([]byte, error) {
	digest = normalizeDigest(digest)
	u := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + digest
	dbg("blob GET %s", u)
	resp, err := doGET(u, map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/octet-stream",
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		dbg("blob GET -> %s", resp.Status)
		return nil, fmt.Errorf("blob %s: %s", digest, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("blob %s: digest mismatch: got %s", digest, got)
	}
	return b, nil
}

func fetchAndApplyLayer(ref ImageRef, token, digest, dest string) error {
	digest = normalizeDigest(digest)
	u := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + digest
	dbg("layer GET %s", u)
	resp, err := doGET(u, map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/octet-stream",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		dbg("layer GET -> %s", resp.Status)
		return fmt.Errorf("blob %s: %s", digest, resp.Status)
	}
	return applyBlob(resp.Body, digest, dest)
}

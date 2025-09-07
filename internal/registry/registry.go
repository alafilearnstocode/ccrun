func fetchBlobWithFallback(ref ImageRef, token, digest string) ([]byte, error) {
	d := normalizeDigest(digest)
	if !strings.HasPrefix(d, "sha256:") {
		d = "sha256:" + d
	}

	u := "https://" + ref.Registry + "/v2/" + ref.Repo + "/blobs/" + d
	log.Printf("fetch blob: %s", u)
	resp, err := doGET(ref.Repo, u, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("blob %s: %s, body=%q", d, resp.Status, string(b))
	}
	return io.ReadAll(resp.Body)
}

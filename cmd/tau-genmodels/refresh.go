package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// sources are the upstream catalogs tau vendors.
//
// They are snapshotted rather than fetched at build time so that generating
// the catalog is reproducible and offline: a build should not silently change
// because a provider added a model this morning, and CI should not fail
// because models.dev is down.
var sources = []struct {
	file string
	url  string
}{
	{"modelsdev.json", "https://models.dev/api.json"},
	{"openrouter.json", "https://openrouter.ai/api/v1/models"},
	{"nvidia.json", "https://integrate.api.nvidia.com/v1/models"},
	{"vercelgateway.json", "https://ai-gateway.vercel.sh/v1/models"},
}

func refreshSources(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}

	for _, s := range sources {
		fmt.Fprintf(os.Stderr, "fetching %s\n", s.url)
		body, err := fetchJSON(client, s.url)
		if err != nil {
			return fmt.Errorf("refreshing %s: %w", s.file, err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, s.file), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fetchJSON downloads and re-encodes, which normalises key order and
// whitespace. Without that, a snapshot refresh produces a diff dominated by
// formatting churn and the actual model changes are impossible to review.
func fetchJSON(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%s did not return JSON: %w", url, err)
	}
	return json.Marshal(v)
}

// Package update implements a deterministic, cached check for a newer
// syntropy release on GitHub. It fetches the latest release tag at most
// once per 24h (persisting the result in ~/.syntropy/config.yaml via the
// config package) and compares it against the running binary's version.
// It has no side effects beyond that HTTP GET and config write — it never
// downloads or installs anything, leaving that to the user.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andrewwormald/syntropy/internal/config"
)

const (
	// DefaultBaseURL is the GitHub API base used when Check is called
	// with an empty baseURL.
	DefaultBaseURL = "https://api.github.com"
	owner          = "andrewwormald"
	repo           = "syntropy"
	// CacheTTL is how long a cached result from a previous Check is
	// trusted before a fresh GitHub API call is made.
	CacheTTL = 24 * time.Hour
)

// Result is the outcome of Check.
type Result struct {
	// Available is true when Latest is a newer version than the one
	// Check was called with.
	Available bool
	// Latest is the latest release version known, e.g. "v0.4.0". Set
	// whether or not a check was actually performed this call (a cached
	// value counts), empty only if no check has ever succeeded.
	Latest string
}

// Check reports whether a newer syntropy release than currentVersion is
// available on GitHub, using the cached result in home's config.yaml when
// it's less than CacheTTL old. On a cache miss it fetches the latest
// release from the GitHub API and persists the result before returning.
//
// hc defaults to http.DefaultClient when nil, and baseURL to
// DefaultBaseURL when empty.
func Check(ctx context.Context, home, currentVersion string, hc *http.Client, baseURL string) (Result, error) {
	cfg, err := config.Load(home)
	if err != nil {
		return Result{}, err
	}

	now := time.Now()
	if !cfg.UpdateCheckedAt.IsZero() && now.Sub(cfg.UpdateCheckedAt) < CacheTTL {
		return Result{
			Available: IsNewer(currentVersion, cfg.UpdateLatestVersion),
			Latest:    cfg.UpdateLatestVersion,
		}, nil
	}

	latest, err := fetchLatestVersion(ctx, hc, baseURL)
	if err != nil {
		return Result{}, err
	}

	cfg.UpdateCheckedAt = now
	cfg.UpdateLatestVersion = latest
	if err := config.Save(home, cfg); err != nil {
		return Result{}, err
	}

	return Result{Available: IsNewer(currentVersion, latest), Latest: latest}, nil
}

// fetchLatestVersion calls GitHub's "latest release" API and returns its
// tag name, e.g. "v0.4.0".
func fetchLatestVersion(ctx context.Context, hc *http.Client, baseURL string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", base, owner, repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("update: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("update: fetch %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("update: decode response from %s: %w", url, err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("update: %s returned no tag_name", url)
	}
	return release.TagName, nil
}

// IsNewer reports whether latest is a newer version than current. Both are
// expected in the form "v1.2.3" (a leading "v" and any trailing
// "-prerelease"/"+build" suffix are ignored, so "0.0.1-scaffold" compares
// as 0.0.1). If either fails to parse as major.minor.patch at all, IsNewer
// conservatively returns false rather than risk a false positive.
func IsNewer(current, latest string) bool {
	cur, ok := parseVersion(current)
	if !ok {
		return false
	}
	lat, ok := parseVersion(latest)
	if !ok {
		return false
	}
	for i := range cur {
		if lat[i] != cur[i] {
			return lat[i] > cur[i]
		}
	}
	return false
}

// parseVersion parses a "v1.2.3"-style string into [major, minor, patch].
// Any "-prerelease" or "+build" suffix on the patch component is dropped.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return out, false
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	// Drop any prerelease/build metadata trailing the patch number.
	if i := strings.IndexAny(parts[2], "-+"); i != -1 {
		parts[2] = parts[2][:i]
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/andrewwormald/syntropy/internal/config"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.4.0", "v0.4.1", true},
		{"v0.4.0", "v0.5.0", true},
		{"v0.4.0", "v1.0.0", true},
		{"v0.4.1", "v0.4.0", false},
		{"v0.4.0", "v0.4.0", false},
		{"0.4.0", "v0.4.1", true},          // no leading "v" on current
		{"0.0.1-scaffold", "v0.4.0", true}, // prerelease suffix stripped, compared numerically
		{"v0.4.0", "not-a-version", false},
		{"v0.4.0", "", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func newReleaseServer(t *testing.T, tagName string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/repos/andrewwormald/syntropy/releases/latest"
		if r.URL.Path != wantPath {
			t.Errorf("unexpected path %q, want %q", r.URL.Path, wantPath)
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			fmt.Fprintf(w, `{"tag_name": %q}`, tagName)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck_FetchesAndCachesOnMiss(t *testing.T) {
	home := t.TempDir()
	srv := newReleaseServer(t, "v0.5.0", http.StatusOK)

	got, err := Check(context.Background(), home, "v0.4.0", srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !got.Available || got.Latest != "v0.5.0" {
		t.Fatalf("got %+v, want Available=true Latest=v0.5.0", got)
	}

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.UpdateLatestVersion != "v0.5.0" {
		t.Fatalf("cfg.UpdateLatestVersion = %q, want v0.5.0", cfg.UpdateLatestVersion)
	}
	if cfg.UpdateCheckedAt.IsZero() {
		t.Fatal("cfg.UpdateCheckedAt was not set")
	}
}

func TestCheck_UsesCacheWithin24h(t *testing.T) {
	home := t.TempDir()
	if err := config.Save(home, config.Config{
		UpdateCheckedAt:     time.Now().Add(-1 * time.Hour),
		UpdateLatestVersion: "v0.9.0",
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"tag_name": "v99.0.0"}`)
	}))
	defer srv.Close()

	got, err := Check(context.Background(), home, "v0.4.0", srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no HTTP calls, got %d", calls)
	}
	if !got.Available || got.Latest != "v0.9.0" {
		t.Fatalf("got %+v, want the cached v0.9.0", got)
	}
}

func TestCheck_RefetchesAfterCacheExpires(t *testing.T) {
	home := t.TempDir()
	if err := config.Save(home, config.Config{
		UpdateCheckedAt:     time.Now().Add(-25 * time.Hour),
		UpdateLatestVersion: "v0.4.0",
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	srv := newReleaseServer(t, "v0.6.0", http.StatusOK)

	got, err := Check(context.Background(), home, "v0.4.0", srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !got.Available || got.Latest != "v0.6.0" {
		t.Fatalf("got %+v, want the freshly-fetched v0.6.0", got)
	}
}

func TestCheck_NoNewerRelease(t *testing.T) {
	home := t.TempDir()
	srv := newReleaseServer(t, "v0.4.0", http.StatusOK)

	got, err := Check(context.Background(), home, "v0.4.0", srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Available {
		t.Fatalf("got Available=true, want false when current == latest")
	}
}

func TestCheck_HTTPErrorPropagates(t *testing.T) {
	home := t.TempDir()
	srv := newReleaseServer(t, "", http.StatusInternalServerError)

	if _, err := Check(context.Background(), home, "v0.4.0", srv.Client(), srv.URL); err == nil {
		t.Fatal("expected an error on HTTP 500, got nil")
	}
}

func TestCheck_DefaultsHTTPClientAndBaseURL(t *testing.T) {
	home := t.TempDir()
	// A blank hc/baseURL must fall back to http.DefaultClient and
	// DefaultBaseURL without panicking. We can't hit the real GitHub API
	// in a unit test, so just confirm defaulting happens by using a
	// context that's already canceled — the request should fail fast
	// with a context error rather than a nil-pointer panic.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Check(ctx, home, "v0.4.0", nil, ""); err == nil {
		t.Fatal("expected an error from a canceled context, got nil")
	}
}

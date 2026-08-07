package gitlab

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewwormald/syntropy/internal/provider"
)

func TestVerifySignature(t *testing.T) {
	p := &Provider{}
	body := []byte(`{"any":"thing"}`)

	tests := []struct {
		name   string
		header string
		secret string
		want   bool
	}{
		{"matching token", "secret-abc", "secret-abc", true},
		{"mismatched token", "wrong", "secret-abc", false},
		{"empty header", "", "secret-abc", false},
		{"empty secret", "secret-abc", "", false},
		{"both empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("X-Gitlab-Token", tc.header)
			}
			if got := p.VerifySignature(h, body, tc.secret); got != tc.want {
				t.Errorf("VerifySignature: want %v, got %v", tc.want, got)
			}
		})
	}
}

// TestVerifySignature_BodyIgnored documents the GitLab quirk: the body is
// not part of the signature input (unlike GitHub's HMAC). Same token, any
// body, same result. ADR-0020 §2 records this trade-off.
func TestVerifySignature_BodyIgnored(t *testing.T) {
	p := &Provider{}
	h := http.Header{}
	h.Set("X-Gitlab-Token", "secret")
	if !p.VerifySignature(h, []byte("body A"), "secret") {
		t.Errorf("body A should verify")
	}
	if !p.VerifySignature(h, []byte("entirely different body B"), "secret") {
		t.Errorf("body B should also verify — GitLab does not sign the body")
	}
}

func TestCreateMR_DraftPrefix(t *testing.T) {
	// We can't hit a real GitLab; assert the title-prefix logic via the
	// body assembly. Reuses the http.Client interception pattern by
	// pointing the Provider at a test server.
	var seenTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// httptest decodes URL paths; check the suffix only.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenTitle, _ = body["title"].(string)
		_, _ = w.Write([]byte(`{"iid":1,"web_url":"https://gitlab/x/-/merge_requests/1"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})

	// Without Draft → title unchanged
	_, _ = p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Migrate logger",
	})
	if seenTitle != "Migrate logger" {
		t.Errorf("plain title: want %q, got %q", "Migrate logger", seenTitle)
	}

	// With Draft → prefix added
	_, _ = p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Migrate logger", Draft: true,
	})
	if seenTitle != "Draft: Migrate logger" {
		t.Errorf("draft title: want %q, got %q", "Draft: Migrate logger", seenTitle)
	}

	// Already-prefixed → not double-prefixed
	_, _ = p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Draft: already", Draft: true,
	})
	if seenTitle != "Draft: already" {
		t.Errorf("double-draft: want %q, got %q", "Draft: already", seenTitle)
	}
}

// Regression: found live on a real run — a duplicate work-phase CreateMR
// call for a branch that already had an open MR (a reconciler re-trigger
// racing genuine progress) got a 409 from GitLab, which the daemon
// treated as fatal even though the actual work was already done
// correctly. CreateMR must absorb this as idempotent success by looking
// up the existing MR for that branch.
func TestCreateMR_DuplicateBranch409_ReturnsExistingMR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":["Another open merge request already exists for this source branch: !82143"]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "source_branch=syntropy%2Fabc%2Fincrement-2"):
			_, _ = w.Write([]byte(`[{"iid":82143,"web_url":"https://gitlab/x/-/merge_requests/82143"}]`))
		default:
			t.Fatalf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	mr, err := p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "syntropy/abc/increment-2", TargetBranch: "main", Title: "Migrate b2c2",
	})
	if err != nil {
		t.Fatalf("CreateMR should absorb the 409 as idempotent success, got error: %v", err)
	}
	if mr.IID != 82143 {
		t.Errorf("want the existing MR's IID 82143, got %d", mr.IID)
	}
	if mr.URL != "https://gitlab/x/-/merge_requests/82143" {
		t.Errorf("want the existing MR's URL, got %q", mr.URL)
	}
}

// A 409 whose lookup finds nothing (or the lookup itself fails) must
// still surface the original error — never silently swallow a real
// conflict just because we couldn't confirm what it was.
func TestCreateMR_409_LookupFindsNothing_SurfacesOriginalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":["Another open merge request already exists for this source branch: !999"]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	_, err := p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "some-branch", TargetBranch: "main", Title: "x",
	})
	if err == nil {
		t.Fatal("want the original 409 surfaced when the lookup finds no matching MR")
	}
}

// A non-409 error must never be treated as the duplicate-branch case.
func TestCreateMR_NonConflictError_NotTreatedAsDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	_, err := p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "x",
	})
	if err == nil {
		t.Fatal("want the 500 surfaced as an error")
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("want the original 500 apiError surfaced unchanged, got %v", err)
	}
}

// TestCreateMR_LabelsAppliedViaFollowUpCall guards against a real GitLab
// quirk found live: a label that doesn't already exist on the project can
// get silently created-but-not-attached when passed inline on MR creation.
// Labels must be applied via a separate PUT using add_labels (additive),
// not baked into the POST /merge_requests body, mirroring the GitHub
// provider's existing two-step pattern.
func TestCreateMR_LabelsAppliedViaFollowUpCall(t *testing.T) {
	var createBody map[string]any
	var labelCall struct {
		method string
		path   string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_, _ = w.Write([]byte(`{"iid":7,"web_url":"https://gitlab/x/-/merge_requests/7"}`))
			return
		}
		labelCall.method = r.Method
		labelCall.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&labelCall.body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	mr, err := p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Migrate logger",
		Labels: []string{"syntropy", "syntropy:abc123"},
	})
	if err != nil {
		t.Fatalf("CreateMR: %v", err)
	}
	if mr.IID != 7 {
		t.Fatalf("MR IID: want 7, got %d", mr.IID)
	}

	if _, ok := createBody["labels"]; ok {
		t.Errorf("labels must not be sent in the creation payload (that's the buggy path); creation body: %+v", createBody)
	}
	if labelCall.method != http.MethodPut {
		t.Fatalf("want a follow-up PUT to apply labels, got method=%q", labelCall.method)
	}
	if !strings.HasSuffix(labelCall.path, "/merge_requests/7") {
		t.Errorf("label call path: want suffix /merge_requests/7, got %q", labelCall.path)
	}
	if got := labelCall.body["add_labels"]; got != "syntropy,syntropy:abc123" {
		t.Errorf("add_labels: want %q, got %q", "syntropy,syntropy:abc123", got)
	}
}

// TestCreateMR_NoLabels_NoFollowUpCall proves an empty Labels slice makes
// no follow-up request at all — only one HTTP call total.
func TestCreateMR_NoLabels_NoFollowUpCall(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_, _ = w.Write([]byte(`{"iid":1,"web_url":"https://gitlab/x/-/merge_requests/1"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if _, err := p.CreateMR(t.Context(), "owner/repo", provider.MRDraft{
		Branch: "b", TargetBranch: "main", Title: "Migrate logger",
	}); err != nil {
		t.Fatalf("CreateMR: %v", err)
	}
	if callCount != 1 {
		t.Errorf("want exactly 1 HTTP call with no labels, got %d", callCount)
	}
}

func TestReactToNote(t *testing.T) {
	var gotPath, gotMethod, gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotName, _ = body["name"].(string)
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ReactToNote(t.Context(), "owner/repo", 42, 99, streamNote, "eyes"); err != nil {
		t.Fatalf("ReactToNote: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	// httptest decodes URL paths, so the escaped "/" in the project ID
	// comes through unescaped here.
	if gotPath != "/api/v4/projects/owner/repo/merge_requests/42/notes/99/award_emoji" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotName != "eyes" {
		t.Errorf("name: want eyes, got %s", gotName)
	}
}

func TestReplyToDiscussion(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotBody, _ = body["body"].(string)
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ReplyToDiscussion(t.Context(), "owner/repo", 42, "disc-abc", "fixed in latest push"); err != nil {
		t.Fatalf("ReplyToDiscussion: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	// httptest decodes URL paths, so the escaped "/" in the project ID
	// comes through unescaped here.
	if gotPath != "/api/v4/projects/owner/repo/merge_requests/42/discussions/disc-abc/notes" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotBody != "fixed in latest push" {
		t.Errorf("body: want %q, got %q", "fixed in latest push", gotBody)
	}
}

func TestListNotesSince_PopulatesDiscussionIDForDiffAndTopLevelNotes(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{
				"id": "disc-diff-1",
				"notes": [
					{"id": 10, "body": "please fix this line", "system": false, "author": {"id": 1, "username": "reviewer", "bot": false}}
				]
			},
			{
				"id": "disc-top-1",
				"individual_note": true,
				"notes": [
					{"id": 11, "body": "looks good overall", "system": false, "author": {"id": 2, "username": "author", "bot": false}}
				]
			},
			{
				"id": "disc-sys-1",
				"notes": [
					{"id": 12, "body": "changed the description", "system": true, "author": {"id": 3, "username": "ghost", "bot": false}}
				]
			}
		]`))
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.ListNotesSince(t.Context(), "owner/repo", 42, provider.NoteCursor{})
	if err != nil {
		t.Fatalf("ListNotesSince: %v", err)
	}
	if !strings.Contains(gotPath, "/api/v4/projects/owner/repo/merge_requests/42/discussions") {
		t.Errorf("path: got %s", gotPath)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 notes (system note filtered out), got %d: %+v", len(got), got)
	}
	if got[0].ID != 10 || got[0].DiscussionID != "disc-diff-1" {
		t.Errorf("diff note: want id=10 discussionID=disc-diff-1, got id=%d discussionID=%s", got[0].ID, got[0].DiscussionID)
	}
	if got[1].ID != 11 || got[1].DiscussionID != "disc-top-1" {
		t.Errorf("top-level note: want id=11 discussionID=disc-top-1, got id=%d discussionID=%s", got[1].ID, got[1].DiscussionID)
	}
}

func TestListNotesSince_FiltersByWatermarkAndSortsAscending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id": "disc-a", "notes": [{"id": 20, "body": "newest", "system": false, "author": {"id": 1, "username": "u", "bot": false}}]},
			{"id": "disc-b", "notes": [{"id": 5, "body": "too old", "system": false, "author": {"id": 1, "username": "u", "bot": false}}]},
			{"id": "disc-c", "notes": [{"id": 15, "body": "middle", "system": false, "author": {"id": 1, "username": "u", "bot": false}}]}
		]`))
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.ListNotesSince(t.Context(), "owner/repo", 42, provider.NoteCursor{Legacy: 10})
	if err != nil {
		t.Fatalf("ListNotesSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 notes above watermark, got %d: %+v", len(got), got)
	}
	if got[0].ID != 15 || got[1].ID != 20 {
		t.Errorf("want ascending [15, 20], got [%d, %d]", got[0].ID, got[1].ID)
	}
}

// --- TokenSource tests (ADR-0063: don't cache a token that can go stale) ---

func TestNew_RequiresTokenOrTokenSource(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error when neither Token nor TokenSource is set")
	}
	if _, err := New(Config{Token: "t"}); err != nil {
		t.Errorf("Token alone should be sufficient: %v", err)
	}
	if _, err := New(Config{TokenSource: func() (string, error) { return "t", nil }}); err != nil {
		t.Errorf("TokenSource alone should be sufficient: %v", err)
	}
}

func TestDo_TokenSource_ResolvedFreshOnEveryRequest(t *testing.T) {
	var gotAuthHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeaders = append(gotAuthHeaders, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"id":1,"username":"andreww"}`))
	}))
	defer srv.Close()

	calls := 0
	tokens := []string{"first-token", "refreshed-token"}
	p, err := New(Config{
		BaseURL: srv.URL,
		TokenSource: func() (string, error) {
			tok := tokens[calls]
			calls++
			return tok, nil
		},
		AuthMode: AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser (1st): %v", err)
	}
	if _, err := p.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser (2nd): %v", err)
	}

	if len(gotAuthHeaders) != 2 {
		t.Fatalf("want 2 requests, got %d", len(gotAuthHeaders))
	}
	if gotAuthHeaders[0] != "Bearer first-token" {
		t.Errorf("1st request: want %q, got %q", "Bearer first-token", gotAuthHeaders[0])
	}
	if gotAuthHeaders[1] != "Bearer refreshed-token" {
		t.Errorf("2nd request: want %q, got %q — TokenSource should be re-resolved every request, not cached", "Bearer refreshed-token", gotAuthHeaders[1])
	}
}

func TestDo_TokenSource_TakesPrecedenceOverStaticToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":1,"username":"andreww"}`))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "stale-static-token",
		TokenSource: func() (string, error) { return "fresh-token", nil },
		AuthMode:    AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser: %v", err)
	}
	if gotAuth != "Bearer fresh-token" {
		t.Errorf("want TokenSource to win over a stale static Token; got %q", gotAuth)
	}
}

func TestDo_TokenSource_ErrorPropagates(t *testing.T) {
	p, err := New(Config{
		BaseURL:     "http://unused.invalid",
		TokenSource: func() (string, error) { return "", errors.New("glab config: no such file") },
		AuthMode:    AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.AuthenticatedUser(t.Context()); err == nil {
		t.Fatal("want error when TokenSource fails, got nil")
	}
}

// --- 401 reactive retry-with-forced-refresh tests (ADR-0078) ---

func TestDo_401_RetriesOnceWithReResolvedToken(t *testing.T) {
	var gotAuthHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeaders = append(gotAuthHeaders, r.Header.Get("Authorization"))
		if len(gotAuthHeaders) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"username":"andreww"}`))
	}))
	defer srv.Close()

	calls := 0
	tokens := []string{"stale-token", "refreshed-token"}
	p, err := New(Config{
		BaseURL: srv.URL,
		TokenSource: func() (string, error) {
			tok := tokens[calls]
			calls++
			return tok, nil
		},
		AuthMode: AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser: want success after retry, got %v", err)
	}
	if len(gotAuthHeaders) != 2 {
		t.Fatalf("want 2 requests (original + retry), got %d", len(gotAuthHeaders))
	}
	if gotAuthHeaders[0] != "Bearer stale-token" {
		t.Errorf("1st request: want %q, got %q", "Bearer stale-token", gotAuthHeaders[0])
	}
	if gotAuthHeaders[1] != "Bearer refreshed-token" {
		t.Errorf("retry: want %q, got %q — tokenSource should be re-invoked on retry", "Bearer refreshed-token", gotAuthHeaders[1])
	}
	if calls != 2 {
		t.Errorf("want tokenSource invoked twice, got %d", calls)
	}
}

func TestDo_401_UnchangedErrorWhenRetryAlsoFails(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	calls := 0
	p, err := New(Config{
		BaseURL: srv.URL,
		TokenSource: func() (string, error) {
			calls++
			return "still-invalid-token", nil
		},
		AuthMode: AuthBearer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.AuthenticatedUser(t.Context())
	if err == nil {
		t.Fatal("want error when the retry also 401s, got nil")
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("want a 401 apiError, got %v", err)
	}
	if requests != 2 {
		t.Errorf("want exactly one retry (2 requests total), got %d", requests)
	}
	if calls != 2 {
		t.Errorf("want tokenSource re-invoked once for the retry, got %d", calls)
	}
}

func TestDo_401_NoRetryWithoutTokenSource(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL, Token: "static-token", AuthMode: AuthBearer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.AuthenticatedUser(t.Context()); err == nil {
		t.Fatal("want error on 401, got nil")
	}
	if requests != 1 {
		t.Errorf("want no retry for a static Token (can't change between attempts), got %d requests", requests)
	}
}

func TestGetMR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/owner/repo/merge_requests/42" {
			t.Errorf("path: got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"source_branch":"feature-x","web_url":"https://gitlab.example/owner/repo/-/merge_requests/42","state":"opened"}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.GetMR(t.Context(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetMR: %v", err)
	}
	want := provider.MR{
		ProjectID: "owner/repo",
		IID:       42,
		URL:       "https://gitlab.example/owner/repo/-/merge_requests/42",
		Branch:    "feature-x",
		State:     "opened",
	}
	if got != want {
		t.Errorf("GetMR: got %+v, want %+v", got, want)
	}
}

func TestGetMR_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if _, err := p.GetMR(t.Context(), "owner/repo", 42); err == nil {
		t.Fatal("want error on 404, got nil")
	}
}

func TestGetMRState_NoConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"opened","has_conflicts":false}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.GetMRState(t.Context(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetMRState: %v", err)
	}
	if got.State != "opened" {
		t.Errorf("state: want opened, got %q", got.State)
	}
	if got.HasConflict {
		t.Errorf("want HasConflict false, got true")
	}
}

func TestGetMRState_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"opened","has_conflicts":true}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.GetMRState(t.Context(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetMRState: %v", err)
	}
	if !got.HasConflict {
		t.Errorf("want HasConflict true, got false")
	}
}

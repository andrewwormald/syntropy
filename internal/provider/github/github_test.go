package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewwormald/syntropy/internal/provider"
)

// sign produces a valid X-Hub-Signature-256 value for body+secret.
func sign(body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_ValidHMAC(t *testing.T) {
	p := &Provider{}
	body := []byte(`{"action":"opened"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(string(body), "secret-abc"))
	if !p.VerifySignature(h, body, "secret-abc") {
		t.Errorf("valid HMAC should verify")
	}
}

func TestVerifySignature_BodyTampered(t *testing.T) {
	p := &Provider{}
	original := []byte(`{"action":"opened"}`)
	tampered := []byte(`{"action":"closed"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(string(original), "secret-abc"))
	if p.VerifySignature(h, tampered, "secret-abc") {
		t.Errorf("tampered body should not verify (signed body was %q, request body is %q)",
			string(original), string(tampered))
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	p := &Provider{}
	body := []byte(`{}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(string(body), "secret-abc"))
	if p.VerifySignature(h, body, "different-secret") {
		t.Errorf("different secret should not verify")
	}
}

func TestVerifySignature_EmptyOrMalformed(t *testing.T) {
	p := &Provider{}
	body := []byte(`{}`)

	cases := []struct {
		name   string
		header string
		secret string
	}{
		{"missing header", "", "secret"},
		{"empty secret", sign(string(body), "secret"), ""},
		{"wrong prefix", "md5=" + hex.EncodeToString([]byte("anything")), "secret"},
		{"no prefix", hex.EncodeToString([]byte("anything")), "secret"},
		{"invalid hex", "sha256=ZZZZZZ", "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("X-Hub-Signature-256", tc.header)
			}
			if p.VerifySignature(h, body, tc.secret) {
				t.Errorf("malformed/missing should not verify")
			}
		})
	}
}

func TestSplitProjectID(t *testing.T) {
	cases := []struct {
		in        string
		owner     string
		repo      string
		expectErr bool
	}{
		{"owner/repo", "owner", "repo", false},
		{"andrewwormald/everflow", "andrewwormald", "everflow", false},
		{"", "", "", true},
		{"only-one-part", "", "", true},
		{"owner/", "", "", true},
		{"/repo", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			o, r, err := splitProjectID(tc.in)
			if tc.expectErr {
				if err == nil {
					t.Errorf("want error for %q, got owner=%q repo=%q", tc.in, o, r)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if o != tc.owner || r != tc.repo {
				t.Errorf("split(%q) = (%q, %q); want (%q, %q)", tc.in, o, r, tc.owner, tc.repo)
			}
		})
	}
}

// EventNoteAdded subscription expands to all three GitHub comment events.
func TestMapEventKinds_NoteAddedExpandsToThreeEvents(t *testing.T) {
	got := mapEventKinds([]provider.EventKind{provider.EventNoteAdded})
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	for _, want := range []string{"issue_comment", "pull_request_review", "pull_request_review_comment"} {
		if !set[want] {
			t.Errorf("EventNoteAdded should subscribe to %q; got %v", want, got)
		}
	}
}

func TestMapEventKinds_PipelineKindsCollapseToCheckSuite(t *testing.T) {
	got := mapEventKinds([]provider.EventKind{
		provider.EventPipelineSucceeded,
		provider.EventPipelineFailed,
	})
	if len(got) != 1 || got[0] != "check_suite" {
		t.Errorf("Pipeline kinds should collapse to [check_suite]; got %v", got)
	}
}

func TestMapEventKinds_MRKindsCollapseToPullRequest(t *testing.T) {
	got := mapEventKinds([]provider.EventKind{
		provider.EventMRMerged,
		provider.EventMRClosed,
		provider.EventMRUpdated,
	})
	if len(got) != 1 || got[0] != "pull_request" {
		t.Errorf("MR kinds should collapse to [pull_request]; got %v", got)
	}
}

// TestResolveDiscussion_TwoStepGraphQL exercises the full happy path:
// list the PR's reviewThreads (with their comments), match discussionID
// against a comment's node id, then call resolveReviewThread on the
// owning thread. The fake GitHub responds with the expected GraphQL
// envelopes for each call.
func TestResolveDiscussion_TwoStepGraphQL(t *testing.T) {
	var (
		gotLookup  bool
		gotResolve bool
		seenOwner  string
		seenRepo   string
		seenNumber float64
		seenThread string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Errorf("want /graphql, got %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		switch {
		case strings.Contains(req.Query, "reviewThreads"):
			gotLookup = true
			seenOwner, _ = req.Variables["owner"].(string)
			seenRepo, _ = req.Variables["repo"].(string)
			seenNumber, _ = req.Variables["number"].(float64)
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"id":"PRRT_other","comments":{"nodes":[{"id":"PRRC_other"}]}},
				{"id":"PRRT_thread_xyz","comments":{"nodes":[{"id":"PRRC_comment_abc"}]}}
			]}}}}}`))
		case strings.Contains(req.Query, "resolveReviewThread"):
			gotResolve = true
			seenThread, _ = req.Variables["threadId"].(string)
			_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":{"thread":{"id":"PRRT_thread_xyz","isResolved":true}}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, "PRRC_comment_abc"); err != nil {
		t.Errorf("ResolveDiscussion: want nil err, got %v", err)
	}
	if !gotLookup {
		t.Errorf("expected the reviewThreads lookup query to fire")
	}
	if !gotResolve {
		t.Errorf("expected the resolveReviewThread mutation to fire")
	}
	if seenOwner != "owner" || seenRepo != "repo" || seenNumber != 42 {
		t.Errorf("lookup vars: want owner=owner repo=repo number=42, got owner=%q repo=%q number=%v", seenOwner, seenRepo, seenNumber)
	}
	if seenThread != "PRRT_thread_xyz" {
		t.Errorf("mutation threadId: want PRRT_thread_xyz, got %q", seenThread)
	}
}

// TestResolveDiscussion_RejectsBrokenNodeShape is a regression test for
// the original, broken query shape: `node(id: $commentId) { ... on
// PullRequestReviewComment { pullRequestReviewThread { id } } }`.
// PullRequestReviewComment has no pullRequestReviewThread field on
// GitHub's live schema, so that query would fail server-side with a
// GraphQL validation error. The fixed implementation must never send a
// query shaped like that.
func TestResolveDiscussion_RejectsBrokenNodeShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		if strings.Contains(req.Query, "pullRequestReviewThread") && strings.Contains(req.Query, "node(id:") {
			t.Errorf("ResolveDiscussion sent the broken node(id:)/pullRequestReviewThread query shape: %s", req.Query)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Field 'pullRequestReviewThread' doesn't exist on type 'PullRequestReviewComment'"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, "PRRC_comment_abc"); err != nil {
		t.Errorf("ResolveDiscussion: want nil err, got %v", err)
	}
}

// TestResolveDiscussion_EmptyID_NoOp guards the caller contract:
// passing an empty discussionID must not fire any HTTP call.
func TestResolveDiscussion_EmptyID_NoOp(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, ""); err != nil {
		t.Errorf("empty discussionID should be a no-op, got err: %v", err)
	}
	if called {
		t.Errorf("empty discussionID must not trigger an HTTP call")
	}
}

// TestResolveDiscussion_NotAThread covers the case where the lookup
// returns no thread containing the comment (e.g. the comment is a
// review-level comment, not an inline one). Treat as no-op rather than
// error so the caller's best-effort resolve doesn't surface noise.
func TestResolveDiscussion_NotAThread(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, "PRRC_xyz"); err != nil {
		t.Errorf("no-thread case: want nil err, got %v", err)
	}
	if calls != 1 {
		t.Errorf("only the lookup should fire when no thread is returned; got %d calls", calls)
	}
}

// TestResolveDiscussion_GraphQLError surfaces GraphQL-level errors as
// Go errors. GitHub returns 200 OK with an `errors` array on permission
// failures and similar — the envelope decoder must promote those.
func TestResolveDiscussion_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"Resource not accessible by integration"}]}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	err := p.ResolveDiscussion(t.Context(), "owner/repo", 42, "PRRC_xyz")
	if err == nil {
		t.Fatalf("want error on GraphQL errors, got nil")
	}
	if !strings.Contains(err.Error(), "Resource not accessible") {
		t.Errorf("err should surface the GraphQL message, got %v", err)
	}
}

// --- GetMRState ---

func TestGetMRState_Open(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/42" {
			t.Errorf("path: got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"state":"open","merged":false}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.GetMRState(t.Context(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetMRState: %v", err)
	}
	if got.State != "opened" {
		t.Errorf("open: want opened, got %q", got.State)
	}
	if got.HasConflict {
		t.Errorf("open: want HasConflict false, got true")
	}
}

func TestGetMRState_Closed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"closed","merged":false}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, _ := p.GetMRState(t.Context(), "owner/repo", 42)
	if got.State != "closed" {
		t.Errorf("closed: want closed, got %q", got.State)
	}
}

func TestGetMRState_Merged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"closed","merged":true}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, _ := p.GetMRState(t.Context(), "owner/repo", 42)
	if got.State != "merged" {
		t.Errorf("merged: want merged, got %q", got.State)
	}
}

func TestGetMRState_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"open","merged":false,"mergeable_state":"dirty"}`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.GetMRState(t.Context(), "owner/repo", 42)
	if err != nil {
		t.Fatalf("GetMRState: %v", err)
	}
	if !got.HasConflict {
		t.Errorf("dirty: want HasConflict true, got false")
	}
	if got.State != "opened" {
		t.Errorf("dirty: want state opened, got %q", got.State)
	}
}

// --- ListNotesSince ---

// TestListNotesSince_MergesThreeStreams covers the headline behaviour:
// the three GitHub comment endpoints (issue comments, inline review
// comments, top-level reviews) are queried and merged into a single
// chronological stream filtered by sinceNoteID.
func TestListNotesSince_MergesThreeStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/issues/42/comments":
			_, _ = w.Write([]byte(`[
				{"id": 10, "body": "old PR comment", "user": {"id":1, "login":"alice", "type":"User"}},
				{"id": 100, "body": "new PR comment", "user": {"id":1, "login":"alice", "type":"User"}}
			]`))
		case "/repos/owner/repo/pulls/42/comments":
			_, _ = w.Write([]byte(`[
				{"id": 5, "node_id": "PRRC_old", "body": "old inline", "user": {"id":2, "login":"bob", "type":"User"}},
				{"id": 200, "node_id": "PRRC_new", "body": "new inline", "user": {"id":2, "login":"bob", "type":"User"}}
			]`))
		case "/repos/owner/repo/pulls/42/reviews":
			_, _ = w.Write([]byte(`[
				{"id": 150, "body": "Looks good but consider X", "state": "COMMENTED", "user": {"id":3, "login":"carol", "type":"User"}},
				{"id": 160, "body": "", "state": "APPROVED", "user": {"id":3, "login":"carol", "type":"User"}}
			]`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.ListNotesSince(t.Context(), "owner/repo", 42, provider.NoteCursor{Legacy: 50})
	if err != nil {
		t.Fatalf("ListNotesSince: %v", err)
	}

	// Legacy=50 (no per-stream cursors yet) → IDs 10 and 5 dropped (old).
	// ID 160 dropped (body-less approval).
	// Remaining: 100 (PR comment), 200 (inline), 150 (review).
	// Sorted ascending: 100, 150, 200.
	wantIDs := []int64{100, 150, 200}
	if len(got) != 3 {
		t.Fatalf("want 3 notes after filter, got %d: %+v", len(got), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("index %d: want ID %d, got %d", i, want, got[i].ID)
		}
	}

	// Discussion ID only on the inline-comment one.
	for _, n := range got {
		switch n.ID {
		case 200:
			if n.DiscussionID != "PRRC_new" {
				t.Errorf("inline note 200 should carry DiscussionID, got %q", n.DiscussionID)
			}
		case 100, 150:
			if n.DiscussionID != "" {
				t.Errorf("non-inline note %d should not carry DiscussionID, got %q", n.ID, n.DiscussionID)
			}
		}
	}
}

// TestListNotesSince_CrossStreamWatermark is the regression test for
// ADR-0041: a review comment (pull_request_review_comment) with a LOWER id
// than an issue comment already delivered on the issue_comment stream must
// still be delivered. GitHub's comment endpoints draw ids from independent
// sequences, so a single shared watermark (since.Legacy alone, mirroring
// the pre-fix behaviour) would incorrectly filter it out as "already seen"
// and drop it silently and permanently.
func TestListNotesSince_CrossStreamWatermark(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/issues/42/comments":
			// Already delivered on a prior tick — high id on its own stream.
			_, _ = w.Write([]byte(`[{"id": 200, "body": "issue comment", "user": {"id":1, "login":"alice", "type":"User"}}]`))
		case "/repos/owner/repo/pulls/42/comments":
			// New inline review comment — lower id, never seen before.
			_, _ = w.Write([]byte(`[{"id": 100, "node_id": "PRRC_1", "body": "inline review comment", "user": {"id":2, "login":"bob", "type":"User"}}]`))
		case "/repos/owner/repo/pulls/42/reviews":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})

	// Simulate: the issue_comment stream has already advanced past 200 on a
	// prior tick; the review_comment stream has not been seen at all yet.
	since := provider.NoteCursor{ByStream: map[string]int64{
		streamIssueComment: 200,
	}}

	got, err := p.ListNotesSince(t.Context(), "owner/repo", 42, since)
	if err != nil {
		t.Fatalf("ListNotesSince: %v", err)
	}

	// The issue comment (id 200) is already-seen on its own stream and must
	// be filtered. The review comment (id 100) is new on ITS stream and
	// must survive despite its id being lower than the issue_comment
	// watermark — that's the bug a single shared watermark would trigger.
	if len(got) != 1 {
		t.Fatalf("want 1 note (the unseen review comment), got %d: %+v", len(got), got)
	}
	if got[0].ID != 100 {
		t.Errorf("want review comment id 100 delivered, got id %d", got[0].ID)
	}
	if got[0].Stream != streamReviewComment {
		t.Errorf("want stream %q, got %q", streamReviewComment, got[0].Stream)
	}
}

// TestListNotesSince_PR30_PollutedLegacyDoesNotFloorUntrackedStream is the
// regression test for PR #30: once ANY stream has its own ByStream entry for
// an MR, Legacy is no longer a safe floor for a still-untracked stream —
// resume() (workflow.go) advances Legacy on every stream's notes, so by the
// time a second stream posts its first comment, Legacy has already been
// pushed past that comment's id by the first stream. Falling back to Legacy
// here reintroduces the exact silent, permanent drop ADR-0041 was meant to
// fix, just one migration step later.
func TestListNotesSince_PR30_PollutedLegacyDoesNotFloorUntrackedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/issues/42/comments":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/owner/repo/pulls/42/comments":
			// review_comment stream's first-ever comment. Its id is lower
			// than Legacy, which was pushed up by the issue_comment stream.
			_, _ = w.Write([]byte(`[{"id": 100, "node_id": "PRRC_1", "body": "first inline comment", "user": {"id":2, "login":"bob", "type":"User"}}]`))
		case "/repos/owner/repo/pulls/42/reviews":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})

	// issue_comment already has its own entry (this MR has been migrated to
	// per-stream tracking); Legacy was advanced past 100 by that stream's
	// notes. review_comment has never posted before, so it has no ByStream
	// entry of its own yet.
	since := provider.NoteCursor{
		ByStream: map[string]int64{streamIssueComment: 500},
		Legacy:   500,
	}

	got, err := p.ListNotesSince(t.Context(), "owner/repo", 42, since)
	if err != nil {
		t.Fatalf("ListNotesSince: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 note (review comment 100, never seen on its own stream), got %d: %+v", len(got), got)
	}
	if got[0].ID != 100 {
		t.Errorf("want review comment id 100 delivered despite id < polluted Legacy, got id %d", got[0].ID)
	}
}

// TestListNotesSince_LegacyFloorAppliesPerStream covers the additive
// migration path: a Run created before ADR-0041 has only since.Legacy set
// (no ByStream entries at all). Every stream must fall back to that single
// scalar as its floor, exactly matching pre-fix behaviour until each stream
// gets its own entry.
func TestListNotesSince_LegacyFloorAppliesPerStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/issues/42/comments":
			_, _ = w.Write([]byte(`[{"id": 50, "body": "old", "user": {"id":1, "login":"a", "type":"User"}}]`))
		case "/repos/owner/repo/pulls/42/comments":
			_, _ = w.Write([]byte(`[{"id": 50, "node_id": "n", "body": "old", "user": {"id":1, "login":"a", "type":"User"}}]`))
		case "/repos/owner/repo/pulls/42/reviews":
			_, _ = w.Write([]byte(`[{"id": 50, "body": "old", "state": "COMMENTED", "user": {"id":1, "login":"a", "type":"User"}}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})

	got, err := p.ListNotesSince(t.Context(), "owner/repo", 42, provider.NoteCursor{Legacy: 50})
	if err != nil {
		t.Fatalf("ListNotesSince: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Legacy floor should apply to every stream with no ByStream entry; want 0 notes, got %+v", got)
	}
}

// TestListNotesSince_EmptyAfterFilter covers the case where all notes
// are older than sinceNoteID.
func TestListNotesSince_EmptyAfterFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "body": "old", "user": {"id":1, "login":"x", "type":"User"}}]`))
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, err := p.ListNotesSince(t.Context(), "owner/repo", 42, provider.NoteCursor{Legacy: 1000})
	if err != nil {
		t.Fatalf("ListNotesSince: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("all notes old; want empty result, got %+v", got)
	}
}

// TestListNotesSince_ReviewWithBody includes a review that has a body
// AND a non-approved state — must surface.
func TestListNotesSince_ReviewWithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			_, _ = w.Write([]byte(`[{"id": 99, "body": "needs work", "state": "CHANGES_REQUESTED", "user": {"id":1, "login":"r", "type":"User"}}]`))
		} else {
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	got, _ := p.ListNotesSince(t.Context(), "owner/repo", 42, provider.NoteCursor{})
	if len(got) != 1 || got[0].ID != 99 {
		t.Errorf("changes-requested review with body should surface; got %+v", got)
	}
}

// --- ReactToNote ---

func TestReactToNote_IssueComment(t *testing.T) {
	var gotPath, gotMethod, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotContent, _ = body["content"].(string)
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ReactToNote(t.Context(), "owner/repo", 42, 99, streamIssueComment, "eyes"); err != nil {
		t.Fatalf("ReactToNote: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	if gotPath != "/repos/owner/repo/issues/comments/99/reactions" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotContent != "eyes" {
		t.Errorf("content: want eyes, got %s", gotContent)
	}
}

func TestReactToNote_ReviewComment(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ReactToNote(t.Context(), "owner/repo", 42, 123, streamReviewComment, "eyes"); err != nil {
		t.Fatalf("ReactToNote: %v", err)
	}
	if gotPath != "/repos/owner/repo/pulls/comments/123/reactions" {
		t.Errorf("path: got %s", gotPath)
	}
}

// TestReactToNote_Review documents that top-level PR reviews have no
// reactions endpoint on GitHub — ReactToNote must no-op rather than error.
func TestReactToNote_Review(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	if err := p.ReactToNote(t.Context(), "owner/repo", 42, 7, streamReview, "eyes"); err != nil {
		t.Fatalf("ReactToNote: %v", err)
	}
	if called {
		t.Errorf("no reactions endpoint should be hit for streamReview")
	}
}

func TestReplyToDiscussion_PostsToRepliesEndpoint(t *testing.T) {
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
	if err := p.ReplyToDiscussion(t.Context(), "owner/repo", 42, "123456", "thanks, fixed"); err != nil {
		t.Fatalf("ReplyToDiscussion: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	if gotPath != "/repos/owner/repo/pulls/42/comments/123456/replies" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotBody != "thanks, fixed" {
		t.Errorf("body: want %q, got %q", "thanks, fixed", gotBody)
	}
}

func TestReplyToDiscussion_NonNumericDiscussionIDErrors(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	p, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	// GraphQL node IDs (e.g. as surfaced via NotePoll.DiscussionID from the
	// inline-comment stream) are not accepted here — this endpoint requires
	// the numeric review-comment ID.
	err := p.ReplyToDiscussion(t.Context(), "owner/repo", 42, "PRRC_kwDOABC123", "thanks, fixed")
	if err == nil {
		t.Fatal("expected an error for a non-numeric discussionID")
	}
	if called {
		t.Errorf("request should not be made when discussionID fails to parse")
	}
}

// TestGraphQLEndpoint_GHE verifies the GHE path-rewrite logic.
func TestGraphQLEndpoint_GHE(t *testing.T) {
	p := &Provider{baseURL: "https://ghe.example.com/api/v3"}
	if got := p.graphQLEndpoint(); got != "https://ghe.example.com/api/graphql" {
		t.Errorf("GHE: want https://ghe.example.com/api/graphql, got %s", got)
	}
	p2 := &Provider{baseURL: "https://api.github.com"}
	if got := p2.graphQLEndpoint(); got != "https://api.github.com/graphql" {
		t.Errorf("github.com: want https://api.github.com/graphql, got %s", got)
	}
}

// --- TokenSource tests (ADR-0067: same staleness fix as GitLab's ADR-0063/0065) ---

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
		_, _ = w.Write([]byte(`{"id":1,"login":"andreww","type":"User"}`))
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
		_, _ = w.Write([]byte(`{"id":1,"login":"andreww","type":"User"}`))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:     srv.URL,
		Token:       "stale-static-token",
		TokenSource: func() (string, error) { return "fresh-token", nil },
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
		TokenSource: func() (string, error) { return "", errors.New("gh auth token: not logged in") },
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
		_, _ = w.Write([]byte(`{"id":1,"login":"andreww","type":"User"}`))
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

	p, err := New(Config{BaseURL: srv.URL, Token: "static-token"})
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

func TestDoGraphQL_TokenSource_ResolvedFreshOnEveryRequest(t *testing.T) {
	var gotAuthHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeaders = append(gotAuthHeaders, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":{}}`))
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
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var out map[string]any
	if err := p.doGraphQL(t.Context(), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatalf("doGraphQL (1st): %v", err)
	}
	if err := p.doGraphQL(t.Context(), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatalf("doGraphQL (2nd): %v", err)
	}

	if len(gotAuthHeaders) != 2 {
		t.Fatalf("want 2 requests, got %d", len(gotAuthHeaders))
	}
	if gotAuthHeaders[1] != "Bearer refreshed-token" {
		t.Errorf("2nd request: want %q, got %q — TokenSource should be re-resolved every request, not cached", "Bearer refreshed-token", gotAuthHeaders[1])
	}
}

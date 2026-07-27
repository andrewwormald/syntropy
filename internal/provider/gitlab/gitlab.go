// Package gitlab implements provider.Provider for GitLab.com and self-hosted
// GitLab instances. See ADR-0020 for the implementation choices (hand-rolled
// HTTP client, bare-token webhook verification, etc.).
package gitlab

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andrewwormald/syntropy/internal/provider"
)

// Provider is the GitLab implementation of provider.Provider.
type Provider struct {
	baseURL     string // e.g. https://gitlab.com
	token       string // personal/project/group access token; ignored if tokenSource is set
	tokenSource func() (string, error) // re-resolves the token on every request; see Config.TokenSource
	authMode    AuthMode
	hc          *http.Client
}

// AuthMode picks the HTTP header GitLab uses to authenticate. PATs go in
// PRIVATE-TOKEN; OAuth tokens (e.g. from `glab` config) go in
// Authorization: Bearer.
type AuthMode int

const (
	AuthPAT    AuthMode = 0 // PRIVATE-TOKEN: <pat>      (default; backwards-compatible)
	AuthBearer AuthMode = 1 // Authorization: Bearer <oauth-token>
)

// Config wires a Provider.
type Config struct {
	BaseURL string // defaults to https://gitlab.com
	// Token is a static personal/project/group access token. Required
	// unless TokenSource is set. PATs don't expire on their own the way an
	// OAuth access token does, so a static string is fine here.
	Token string
	// TokenSource, if set, takes precedence over Token and is called to
	// resolve the bearer/PAT value fresh on every request instead of
	// caching one at construction time. Use this for tokens that can go
	// stale behind the Provider's back — e.g. `glab auth login`'s OAuth
	// access token, which `glab` itself transparently refreshes in its own
	// config file (see LoadGlabToken and ADR-0063): a Provider built with a
	// one-time Token snapshot would keep using an expired access token
	// forever, since it has no way to notice `glab` refreshed a new one.
	TokenSource func() (string, error)
	AuthMode    AuthMode      // defaults to AuthPAT (the v1 behaviour)
	Timeout     time.Duration // defaults to 30s per request
}

// New constructs a Provider. Returns an error if neither Token nor
// TokenSource is set so callers fail fast at daemon start rather than
// discovering at first API call.
func New(cfg Config) (*Provider, error) {
	if cfg.Token == "" && cfg.TokenSource == nil {
		return nil, errors.New("gitlab: Token or TokenSource is required (set GITLAB_TOKEN)")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://gitlab.com"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Provider{
		baseURL:     base,
		token:       cfg.Token,
		tokenSource: cfg.TokenSource,
		authMode:    cfg.AuthMode,
		hc:          &http.Client{Timeout: timeout},
	}, nil
}

// resolveToken returns the token to send with the next request: freshly
// resolved via tokenSource if set, otherwise the static token from Config.
func (p *Provider) resolveToken() (string, error) {
	if p.tokenSource != nil {
		tok, err := p.tokenSource()
		if err != nil {
			return "", fmt.Errorf("resolve token: %w", err)
		}
		return tok, nil
	}
	return p.token, nil
}

func (p *Provider) Name() string { return "gitlab" }

// AuthenticatedUser → GET /api/v4/user.
func (p *Provider) AuthenticatedUser(ctx context.Context) (provider.User, error) {
	var raw struct {
		ID       int    `json:"id"`
		Username string `json:"username"`
		Email    string `json:"email"`
		Bot      bool   `json:"bot"`
	}
	if err := p.doJSON(ctx, http.MethodGet, "/api/v4/user", nil, &raw); err != nil {
		return provider.User{}, err
	}
	return provider.User{
		ID:     fmt.Sprintf("%d", raw.ID),
		Handle: raw.Username,
		Email:  raw.Email,
		Bot:    raw.Bot,
	}, nil
}

// RegisterWebhook → POST /api/v4/projects/:id/hooks. Event flags map onto
// GitLab's webhook event toggles. Idempotency is the caller's job (workflow
// state tracks WebhookID); GitLab will happily create duplicate hooks.
func (p *Provider) RegisterWebhook(ctx context.Context, projectID, callbackURL, secret string, events []provider.EventKind) (string, error) {
	body := map[string]any{
		"url":         callbackURL,
		"token":       secret,
		"enable_ssl_verification": true,
	}
	for _, k := range events {
		switch k {
		case provider.EventNoteAdded:
			body["note_events"] = true
			body["confidential_note_events"] = true
		case provider.EventPipelineSucceeded, provider.EventPipelineFailed:
			body["pipeline_events"] = true
		case provider.EventMRMerged, provider.EventMRClosed, provider.EventMRUpdated:
			body["merge_requests_events"] = true
		}
	}
	var resp struct {
		ID int `json:"id"`
	}
	path := fmt.Sprintf("/api/v4/projects/%s/hooks", url.PathEscape(projectID))
	if err := p.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", resp.ID), nil
}

// DeregisterWebhook → DELETE /api/v4/projects/:id/hooks/:hook_id. Idempotent
// (404 treated as success — already gone).
func (p *Provider) DeregisterWebhook(ctx context.Context, projectID, webhookID string) error {
	path := fmt.Sprintf("/api/v4/projects/%s/hooks/%s", url.PathEscape(projectID), url.PathEscape(webhookID))
	resp, err := p.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 400 {
		return p.errFromResponse(resp)
	}
	return nil
}

// VerifySignature checks the X-Gitlab-Token header against the registered
// secret using constant-time comparison. GitLab does *not* use HMAC — the
// header is the bare token. The token comparison is the only auth we have
// for inbound webhooks, so do it carefully. See ADR-0020.
func (p *Provider) VerifySignature(headers http.Header, _ []byte, secret string) bool {
	got := headers.Get("X-Gitlab-Token")
	if got == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// CreateMR → POST /api/v4/projects/:id/merge_requests. GitLab signals
// Draft MRs via a "Draft: " title prefix (the modern replacement for
// "WIP:"); we add it here when the caller asks for one.
func (p *Provider) CreateMR(ctx context.Context, projectID string, draft provider.MRDraft) (provider.MR, error) {
	title := draft.Title
	if draft.Draft && !strings.HasPrefix(title, "Draft:") && !strings.HasPrefix(title, "WIP:") {
		title = "Draft: " + title
	}
	body := map[string]any{
		"source_branch": draft.Branch,
		"target_branch": draft.TargetBranch,
		"title":         title,
		"description":   draft.Description,
	}
	var resp struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests", url.PathEscape(projectID))
	if err := p.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return provider.MR{}, err
	}
	mr := provider.MR{
		ProjectID: projectID,
		IID:       resp.IID,
		URL:       resp.WebURL,
		Branch:    draft.Branch,
	}
	if len(draft.Labels) > 0 {
		// A follow-up call, not part of the creation payload above: GitLab
		// can silently create-but-not-attach a label that doesn't already
		// exist on the project when it's passed inline on MR creation —
		// found live (the label ends up existing as a project label, with
		// zero "add" events ever recorded against the MR it was meant for).
		// add_labels (additive) also avoids clobbering any labels gitStream
		// or another integration adds around the same time, unlike the
		// plain labels param (which replaces the whole set). Matches the
		// GitHub provider's existing two-step pattern for the same reason.
		labelPath := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d",
			url.PathEscape(projectID), resp.IID)
		_ = p.doJSON(ctx, http.MethodPut, labelPath, map[string]any{
			"add_labels": strings.Join(draft.Labels, ","),
		}, nil)
		// Label-application failures are non-fatal; the MR is already open.
	}
	return mr, nil
}

// PostComment → POST /api/v4/projects/:id/merge_requests/:iid/notes.
func (p *Provider) PostComment(ctx context.Context, projectID string, mrIID int, body string) error {
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/notes",
		url.PathEscape(projectID), mrIID)
	return p.doJSON(ctx, http.MethodPost, path, map[string]any{"body": body}, nil)
}

// ReplyToDiscussion → POST /api/v4/projects/:id/merge_requests/:iid/discussions/:discussion_id/notes.
// Posts within the existing thread rather than as a new top-level note, so
// the reply shows up nested under the comment it addresses.
func (p *Provider) ReplyToDiscussion(ctx context.Context, projectID string, mrIID int, discussionID string, body string) error {
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/discussions/%s/notes",
		url.PathEscape(projectID), mrIID, url.PathEscape(discussionID))
	return p.doJSON(ctx, http.MethodPost, path, map[string]any{"body": body}, nil)
}

// UpdateMRTitle → PUT /api/v4/projects/:id/merge_requests/:iid.
func (p *Provider) UpdateMRTitle(ctx context.Context, projectID string, mrIID int, title string) error {
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d",
		url.PathEscape(projectID), mrIID)
	return p.doJSON(ctx, http.MethodPut, path, map[string]any{"title": title}, nil)
}

// CloseMR → PUT /api/v4/projects/:id/merge_requests/:iid with state_event=close.
func (p *Provider) CloseMR(ctx context.Context, projectID string, mrIID int) error {
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d",
		url.PathEscape(projectID), mrIID)
	return p.doJSON(ctx, http.MethodPut, path, map[string]any{"state_event": "close"}, nil)
}

// GetMRState → GET /api/v4/projects/:id/merge_requests/:iid. Returns the
// MR's current `state` field ("opened" | "closed" | "merged" | "locked")
// plus `has_conflicts`, used by the poller to detect lifecycle transitions
// and merge conflicts from the same request.
func (p *Provider) GetMRState(ctx context.Context, projectID string, mrIID int) (provider.MRState, error) {
	var resp struct {
		State        string `json:"state"`
		HasConflicts bool   `json:"has_conflicts"`
	}
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d",
		url.PathEscape(projectID), mrIID)
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return provider.MRState{}, err
	}
	return provider.MRState{State: resp.State, HasConflict: resp.HasConflicts}, nil
}

// streamNote is GitLab's single comment stream — unlike GitHub, all MR
// notes come from one endpoint with one monotonic id sequence, so there's
// no cross-stream watermark hazard here. Kept as an explicit key (rather
// than leaving NoteCursor.ByStream empty) so a provider.NoteCursor built
// from AgentState behaves the same way for both providers.
const streamNote = "note"

// ListNotesSince → GET /api/v4/projects/:id/merge_requests/:iid/notes.
// Returns notes whose `id` exceeds the watermark (i.e. arrived since the
// last poll). The poller stores the highest id seen on AgentState.
func (p *Provider) ListNotesSince(ctx context.Context, projectID string, mrIID int, since provider.NoteCursor) ([]provider.NotePoll, error) {
	sinceNoteID, ok := since.ByStream[streamNote]
	if !ok {
		sinceNoteID = since.Legacy
	}
	// GitLab's /notes endpoint only accepts order_by ∈ {created_at,
	// updated_at} (sending order_by=id returns 400). We use the default
	// (created_at) sort=desc and filter by id > sinceNoteID client-side —
	// note IDs are monotonic per-MR so this is equivalent.
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/notes?sort=desc&per_page=50",
		url.PathEscape(projectID), mrIID)
	var raw []struct {
		ID           int64  `json:"id"`
		Body         string `json:"body"`
		System       bool   `json:"system"`        // system notes (state changes etc.) — skip
		DiscussionID string `json:"discussion_id"` // present on regular MR notes since GitLab 13.x
		Author       struct {
			ID       int    `json:"id"`
			Username string `json:"username"`
			Bot      bool   `json:"bot"`
		} `json:"author"`
	}
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	// Filter to non-system, id > sinceNoteID; return in ascending id order.
	out := make([]provider.NotePoll, 0, len(raw))
	for _, n := range raw {
		if n.System || n.ID <= sinceNoteID {
			continue
		}
		out = append(out, provider.NotePoll{
			ID:           n.ID,
			Body:         n.Body,
			DiscussionID: n.DiscussionID,
			Author: provider.User{
				ID:     fmt.Sprintf("%d", n.Author.ID),
				Handle: n.Author.Username,
				Bot:    n.Author.Bot,
			},
			Stream: streamNote,
		})
	}
	// Reverse to ascending so callers process in chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// RetryPipelineJob → POST /api/v4/projects/:id/jobs/:job_id/retry. Used by
// the deterministic CI-flake-retry path; the agent isn't involved.
func (p *Provider) RetryPipelineJob(ctx context.Context, projectID string, jobID int64) error {
	path := fmt.Sprintf("/api/v4/projects/%s/jobs/%d/retry",
		url.PathEscape(projectID), jobID)
	return p.doJSON(ctx, http.MethodPost, path, nil, nil)
}

// ResolveDiscussion → PUT /api/v4/projects/:id/merge_requests/:iid/discussions/:discussion_id?resolved=true.
// Marks the thread as resolved (collapsed in the UI) — called after the
// agent successfully pushes a change addressing a reviewer comment.
// Empty discussionID is a no-op so callers don't need to guard.
func (p *Provider) ResolveDiscussion(ctx context.Context, projectID string, mrIID int, discussionID string) error {
	if discussionID == "" {
		return nil
	}
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/discussions/%s?resolved=true",
		url.PathEscape(projectID), mrIID, url.PathEscape(discussionID))
	return p.doJSON(ctx, http.MethodPut, path, nil, nil)
}

// ReactToNote → POST /api/v4/projects/:id/merge_requests/:iid/notes/:note_id/award_emoji.
// GitLab has a single notes endpoint (see streamNote), so stream is unused —
// kept in the signature for parity with GitHub, which needs it to pick an
// endpoint. See ADR-0050.
func (p *Provider) ReactToNote(ctx context.Context, projectID string, mrIID int, noteID int64, _, emoji string) error {
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/notes/%d/award_emoji",
		url.PathEscape(projectID), mrIID, noteID)
	return p.doJSON(ctx, http.MethodPost, path, map[string]any{"name": emoji}, nil)
}

// IsBot inspects the user.bot field set by GitLab on bot accounts.
// Some long-lived integrations (Danger, sonar) use regular accounts; the
// caller can layer name-pattern matching on top if needed.
func (p *Provider) IsBot(u provider.User) bool { return u.Bot }

// --- HTTP helpers ---

func (p *Provider) doJSON(ctx context.Context, method, path string, in, out any) error {
	resp, err := p.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return p.errFromResponse(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// do issues a request and, on a 401, retries exactly once with a freshly
// re-resolved token before giving up (see ADR-0078). tokenSource already
// re-resolves on every call (ADR-0063), which for GitLab means re-invoking
// RefreshGlabToken (ADR-0065) — so simply calling doOnce a second time is
// enough to force a fresh refresh attempt. A 401 is ambiguous: it can mean
// glab's own access token went stale between its lazy refresh cycles, or
// that a genuine interactive re-login is required. Only the latter should
// reach the poller's auth-backoff/pause path, so this retry silently
// absorbs the former case. No retry without a tokenSource: a static Token
// can't change between attempts, so retrying would just repeat the same
// 401 for no benefit.
func (p *Provider) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	resp, err := p.doOnce(ctx, method, path, in)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized || p.tokenSource == nil {
		return resp, nil
	}
	_ = resp.Body.Close()
	return p.doOnce(ctx, method, path, in)
}

func (p *Provider) doOnce(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	tok, err := p.resolveToken()
	if err != nil {
		return nil, err
	}
	switch p.authMode {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+tok)
	default:
		req.Header.Set("PRIVATE-TOKEN", tok)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return p.hc.Do(req)
}

// apiError carries GitLab's structured error responses for surfacing useful
// debugging info up the stack.
type apiError struct {
	Status int
	Path   string
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("gitlab API %s: status=%d body=%s", e.Path, e.Status, e.Body)
}

func (p *Provider) errFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &apiError{
		Status: resp.StatusCode,
		Path:   resp.Request.URL.Path,
		Body:   strings.TrimSpace(string(body)),
	}
}

// Verify Provider satisfies provider.Provider at compile time.
var _ provider.Provider = (*Provider)(nil)

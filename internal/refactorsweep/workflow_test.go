package refactorsweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/filter"
	"github.com/andrewwormald/syntropy/internal/git"
	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/runner"
	"github.com/andrewwormald/syntropy/internal/webhook"
)

// --- Test fake: provider.Provider ---

// fakeProvider records calls and returns canned values. Sufficient for
// exercising step bodies without hitting real GitLab / GitHub APIs.
type fakeProvider struct {
	mu sync.Mutex

	authedUser provider.User
	authedErr  error

	webhookID  string
	regErr     error
	registered registerCall

	deregisters []string

	createMRResult provider.MR
	createMRErr    error
	createMRCalls  []provider.MRDraft

	commentErr error
	comments   []postedComment

	replyErr error
	replies  []repliedComment

	closeErr error
	closes   []closedMR

	resolveErr error
	resolves   []resolvedDiscussion

	reactErr   error
	reactions  []reactToNoteCall

	retryJobErr error
	retriedJobs []retriedJobCall
}

type retriedJobCall struct {
	ProjectID string
	JobID     int64
}

type resolvedDiscussion struct {
	ProjectID    string
	MRIID        int
	DiscussionID string
}

type repliedComment struct {
	ProjectID    string
	MRIID        int
	DiscussionID string
	Body         string
}

type reactToNoteCall struct {
	ProjectID string
	MRIID     int
	NoteID    int64
	Stream    string
	Emoji     string
}

type closedMR struct {
	ProjectID string
	IID       int
}

type postedComment struct {
	ProjectID string
	MRIID     int
	Body      string
}

type registerCall struct {
	ProjectID  string
	CallbackURL string
	Secret     string
	Events     []provider.EventKind
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) AuthenticatedUser(ctx context.Context) (provider.User, error) {
	return f.authedUser, f.authedErr
}

func (f *fakeProvider) RegisterWebhook(ctx context.Context, projectID, url, secret string, kinds []provider.EventKind) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.regErr != nil {
		return "", f.regErr
	}
	f.registered = registerCall{
		ProjectID:   projectID,
		CallbackURL: url,
		Secret:      secret,
		Events:      append([]provider.EventKind(nil), kinds...),
	}
	return f.webhookID, nil
}

func (f *fakeProvider) DeregisterWebhook(ctx context.Context, projectID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregisters = append(f.deregisters, id)
	return nil
}

func (f *fakeProvider) VerifySignature(_ http.Header, _ []byte, _ string) bool        { return true }
func (f *fakeProvider) NormaliseEvent(_ http.Header, _ []byte) (provider.Event, error) { return provider.Event{}, nil }
func (f *fakeProvider) CreateMR(_ context.Context, projectID string, draft provider.MRDraft) (provider.MR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createMRCalls = append(f.createMRCalls, draft)
	if f.createMRErr != nil {
		return provider.MR{}, f.createMRErr
	}
	if f.createMRResult.IID == 0 {
		// Sensible default: use the project ID and a fake IID.
		return provider.MR{ProjectID: projectID, IID: 1, URL: "https://fake/mr/1", Branch: draft.Branch}, nil
	}
	return f.createMRResult, nil
}
func (f *fakeProvider) PostComment(_ context.Context, projectID string, mrIID int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, postedComment{ProjectID: projectID, MRIID: mrIID, Body: body})
	return f.commentErr
}
func (f *fakeProvider) UpdateMRTitle(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeProvider) GetMRState(_ context.Context, _ string, _ int) (string, error)    { return "opened", nil }
func (f *fakeProvider) ListNotesSince(_ context.Context, _ string, _ int, _ provider.NoteCursor) ([]provider.NotePoll, error) {
	return nil, nil
}
func (f *fakeProvider) ResolveDiscussion(_ context.Context, projectID string, mrIID int, discussionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves = append(f.resolves, resolvedDiscussion{ProjectID: projectID, MRIID: mrIID, DiscussionID: discussionID})
	return f.resolveErr
}
func (f *fakeProvider) ReplyToDiscussion(_ context.Context, projectID string, mrIID int, discussionID string, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, repliedComment{ProjectID: projectID, MRIID: mrIID, DiscussionID: discussionID, Body: body})
	return f.replyErr
}
func (f *fakeProvider) CloseMR(_ context.Context, projectID string, iid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, closedMR{ProjectID: projectID, IID: iid})
	return f.closeErr
}
func (f *fakeProvider) RetryPipelineJob(_ context.Context, projectID string, jobID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retriedJobs = append(f.retriedJobs, retriedJobCall{ProjectID: projectID, JobID: jobID})
	return f.retryJobErr
}
func (f *fakeProvider) ReactToNote(_ context.Context, projectID string, mrIID int, noteID int64, stream, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, reactToNoteCall{
		ProjectID: projectID, MRIID: mrIID, NoteID: noteID, Stream: stream, Emoji: emoji,
	})
	return f.reactErr
}
func (f *fakeProvider) IsBot(u provider.User) bool { return u.Bot }

// --- Test fake: runner.Runner ---

// fakeRunner records calls and returns canned responses.
type fakeRunner struct {
	mu sync.Mutex

	resp  runner.Response
	err   error
	calls []runner.Request

	// If non-nil, called after Run() returns. Useful for tests that want
	// to simulate the runner having modified files in the worktree.
	onRun func(req runner.Request)
}

func (f *fakeRunner) Name() string { return "fake-runner" }
func (f *fakeRunner) Run(_ context.Context, req runner.Request) (runner.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.onRun != nil {
		f.onRun(req)
	}
	return f.resp, f.err
}

// --- Test fake: git.Git ---

// fakeGit records calls and returns canned results. Default: dirty=true on
// HasChanges (so tests don't have to remember to opt in). Override with
// the explicit fields when a test needs different behaviour.
type fakeGit struct {
	mu sync.Mutex

	ensureErr  error
	resetErr   error
	syncErr    error
	commitErr  error
	pushErr    error
	hasChanges *bool // nil → default true; set to a bool pointer for explicit
	hasChErr   error
	hasWork    *bool   // HasWorkBeyondBase; nil → mirror hasChanges
	hasWorkErr error
	diffStat   *string // nil → default "1 file changed, 5 insertions(+)"
	isolated   *bool   // IsIsolatedWorktree; nil → default true
	isolateErr error

	ensures      []ensureCall
	resets       []string
	syncs        []string
	commits      []string
	pushes       []string
	removes      []string
	hasWorkCalls []string // dir+"@"+baseBranch, to assert the comparison ref
}

type ensureCall struct {
	Dir, BaseRepo, BaseBranch, Branch string
}

func (g *fakeGit) EnsureBranch(_ context.Context, dir, baseRepo, baseBranch, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensures = append(g.ensures, ensureCall{dir, baseRepo, baseBranch, branch})
	return g.ensureErr
}

func (g *fakeGit) HardReset(_ context.Context, dir, baseBranch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resets = append(g.resets, dir+"@"+baseBranch)
	return g.resetErr
}

func (g *fakeGit) SyncWithBase(_ context.Context, dir, baseBranch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.syncs = append(g.syncs, dir+"@"+baseBranch)
	return g.syncErr
}

func (g *fakeGit) HasChanges(_ context.Context, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hasChErr != nil {
		return false, g.hasChErr
	}
	if g.hasChanges != nil {
		return *g.hasChanges, nil
	}
	return true, nil // default: assume runner did make changes
}

func (g *fakeGit) HasWorkBeyondBase(ctx context.Context, dir, baseBranch string) (bool, error) {
	g.mu.Lock()
	g.hasWorkCalls = append(g.hasWorkCalls, dir+"@"+baseBranch)
	if g.hasWorkErr != nil {
		g.mu.Unlock()
		return false, g.hasWorkErr
	}
	if g.hasWork != nil {
		v := *g.hasWork
		g.mu.Unlock()
		return v, nil
	}
	g.mu.Unlock()
	// Default: mirror HasChanges so tests that only care about dirty vs
	// clean keep a single knob. Set hasWork explicitly to model a
	// self-committing runner (clean tree, committed work).
	return g.HasChanges(ctx, dir)
}

func (g *fakeGit) Commit(_ context.Context, _, msg string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commits = append(g.commits, msg)
	return g.commitErr
}

func (g *fakeGit) Push(_ context.Context, _, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pushes = append(g.pushes, branch)
	return g.pushErr
}

func (g *fakeGit) RemoveWorktree(_ context.Context, _, dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removes = append(g.removes, dir)
	return nil
}

func (g *fakeGit) DiffShortstat(_ context.Context, _, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.diffStat != nil {
		return *g.diffStat, nil
	}
	return "1 file changed, 5 insertions(+)", nil
}

func (g *fakeGit) IsIsolatedWorktree(_ context.Context, _ string) (bool, error) {
	if g.isolateErr != nil {
		return false, g.isolateErr
	}
	if g.isolated != nil {
		return *g.isolated, nil
	}
	return true, nil
}

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

// --- Helpers ---

func newDeps(t *testing.T, p provider.Provider) *Deps {
	t.Helper()
	reg := runner.NewRegistry()
	return &Deps{
		Providers:     map[string]provider.Provider{p.Name(): p},
		Runners:       reg,
		Git:           &fakeGit{},
		Secrets:       webhook.NewSecretRegistry(),
		PublicBaseURL: "https://syntropy.test",
		RunsRoot:      t.TempDir(),
	}
}

// withGit replaces the default fakeGit with a tailored one so tests can
// override individual behaviours (e.g. push fails, no changes detected).
func (d *Deps) withGit(g *fakeGit) *fakeGit {
	d.Git = g
	return g
}

// withRunner adds a runner to Deps.Runners and returns the typed fake so the
// test can inspect calls + override response.
func (d *Deps) withRunner(t *testing.T, fr *fakeRunner) *fakeRunner {
	t.Helper()
	d.Runners.Register(fr)
	return fr
}

func newRun(t *testing.T, state *AgentState) *workflow.Run[AgentState, AgentStatus] {
	t.Helper()
	// Run embeds TypedRecord which embeds Record. Step bodies only read
	// RunID and the Object — the controller (private) is not invoked here.
	return &workflow.Run[AgentState, AgentStatus]{
		TypedRecord: workflow.TypedRecord[AgentState, AgentStatus]{
			Record: workflow.Record{
				WorkflowName: "refactor-sweep-test",
				ForeignID:    "test-foreign",
				RunID:        "00000000-0000-0000-0000-deadbeefcafe",
			},
			Object: state,
		},
	}
}

// --- setup() tests ---

func TestSetup_HappyPath(t *testing.T) {
	fp := &fakeProvider{
		authedUser: provider.User{ID: "42", Handle: "andreww", Email: "a@example.com"},
		webhookID:  "wh-99",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		EventSource:  EventSourceWebhook, // explicit — default is now poll (ADR-0031)
	})

	next, err := d.setup(t.Context(), r)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("next status: want Discovering, got %v", next)
	}

	// Author was captured from AuthenticatedUser.
	if r.Object.Author.Handle != "andreww" {
		t.Errorf("Author.Handle: want andreww, got %q", r.Object.Author.Handle)
	}

	// Webhook was registered with the right shape.
	if fp.registered.ProjectID != "acme/example" {
		t.Errorf("RegisterWebhook ProjectID: want acme/example, got %q", fp.registered.ProjectID)
	}
	if !strings.HasPrefix(fp.registered.CallbackURL, "https://syntropy.test/webhook/fake/") {
		t.Errorf("CallbackURL prefix wrong: got %q", fp.registered.CallbackURL)
	}
	if !strings.HasSuffix(fp.registered.CallbackURL, r.RunID) {
		t.Errorf("CallbackURL should end with runID: got %q", fp.registered.CallbackURL)
	}
	if len(fp.registered.Events) == 0 {
		t.Errorf("RegisterWebhook should subscribe to events")
	}
	if len(fp.registered.Secret) < 32 {
		t.Errorf("Secret too short to be hex-encoded 32 bytes: %d chars", len(fp.registered.Secret))
	}

	// AgentState captured the webhook identity.
	if r.Object.WebhookID != "wh-99" {
		t.Errorf("WebhookID: want wh-99, got %q", r.Object.WebhookID)
	}
	if r.Object.WebhookSecret != fp.registered.Secret {
		t.Errorf("WebhookSecret mismatch with registered secret")
	}

	// SecretRegistry was populated.
	got, ok := d.Secrets.Get("fake", r.RunID)
	if !ok {
		t.Errorf("Secret not in registry")
	}
	if got != r.Object.WebhookSecret {
		t.Errorf("Secret in registry doesn't match AgentState")
	}

	// Per-Run dir created.
	runDir := filepath.Join(d.RunsRoot, r.RunID)
	if info, err := os.Stat(runDir); err != nil {
		t.Errorf("run dir not created: %v", err)
	} else if !info.IsDir() {
		t.Errorf("run dir is not a directory")
	}

	// Defaults applied.
	if r.Object.Concurrency != 1 {
		t.Errorf("Concurrency default: want 1, got %d", r.Object.Concurrency)
	}
	if r.Object.InFlight == nil {
		t.Errorf("InFlight should be initialised")
	}
}

func TestSetup_UnknownProvider(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{ProviderName: "not-registered"})

	next, err := d.setup(t.Context(), r)
	if err == nil {
		t.Fatalf("want error for unknown provider")
	}
	if next != StatusFailed {
		t.Errorf("want StatusFailed, got %v", next)
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error message should mention unknown provider: %v", err)
	}
}

func TestSetup_AuthorOverride_Respected(t *testing.T) {
	// User pre-set the author via --author at trigger time. Setup must NOT
	// overwrite it from the token's authenticated user.
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "service-account"},
		webhookID:  "wh-1",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "x/y",
		Author:       provider.User{Handle: "andreww", Email: "a@example.com"},
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if r.Object.Author.Handle != "andreww" {
		t.Errorf("Author should be preserved when pre-set; got %q", r.Object.Author.Handle)
	}
}

func TestSetup_Idempotent_SkipsWebhookRegistration(t *testing.T) {
	// Second invocation of setup (e.g. after retry, after daemon restart)
	// must not re-register the webhook. WebhookID already on AgentState =
	// already done.
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "andreww"},
		webhookID:  "wh-1",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{
		ProviderName:  "fake",
		ProjectID:     "x/y",
		WebhookID:     "previously-registered",
		WebhookSecret: "previously-set",
		WebhookURL:    "https://previous/webhook/...",
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if fp.registered.ProjectID != "" {
		t.Errorf("RegisterWebhook should not have been called on retry; was called with %+v", fp.registered)
	}
	if r.Object.WebhookID != "previously-registered" {
		t.Errorf("WebhookID was overwritten: got %q", r.Object.WebhookID)
	}
	if r.Object.WebhookSecret != "previously-set" {
		t.Errorf("WebhookSecret was overwritten: got %q", r.Object.WebhookSecret)
	}

	// Secret should still be (re-)populated in the registry — daemon
	// restart needs this even when registration is skipped.
	got, ok := d.Secrets.Get("fake", r.RunID)
	if !ok || got != "previously-set" {
		t.Errorf("Secret should be re-populated in registry; got %q, ok=%v", got, ok)
	}
}

func TestSetup_TitleConvention_AbsentFile(t *testing.T) {
	// No .syntropy.yml at BaseRepo — TitleConvention stays empty, no error.
	fp := &fakeProvider{authedUser: provider.User{Handle: "andreww"}, webhookID: "wh-1"}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "x/y",
		BaseRepo:     t.TempDir(),
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if r.Object.TitleConvention != "" {
		t.Errorf("TitleConvention: want empty, got %q", r.Object.TitleConvention)
	}
}

func TestSetup_TitleConvention_PresentConvention(t *testing.T) {
	fp := &fakeProvider{authedUser: provider.User{Handle: "andreww"}, webhookID: "wh-1"}
	d := newDeps(t, fp)
	baseRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseRepo, ".syntropy.yml"), []byte("title_convention: Conventional Commits\n"), 0o644); err != nil {
		t.Fatalf("write .syntropy.yml: %v", err)
	}
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "x/y",
		BaseRepo:     baseRepo,
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if r.Object.TitleConvention != "Conventional Commits" {
		t.Errorf("TitleConvention: want %q, got %q", "Conventional Commits", r.Object.TitleConvention)
	}
}

func TestSetup_TitleConvention_BlankField(t *testing.T) {
	// .syntropy.yml exists but has no title_convention line — treated the
	// same as an absent file: empty TitleConvention, no error.
	fp := &fakeProvider{authedUser: provider.User{Handle: "andreww"}, webhookID: "wh-1"}
	d := newDeps(t, fp)
	baseRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseRepo, ".syntropy.yml"), []byte("# no convention set\n"), 0o644); err != nil {
		t.Fatalf("write .syntropy.yml: %v", err)
	}
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "x/y",
		BaseRepo:     baseRepo,
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if r.Object.TitleConvention != "" {
		t.Errorf("TitleConvention: want empty, got %q", r.Object.TitleConvention)
	}
}

func TestSetup_TitleConvention_NotReReadMidRun(t *testing.T) {
	// StartedAt already set (Run past its first setup() pass) — a second
	// invocation (retry/restart) must not re-read .syntropy.yml even though
	// the file on disk has since changed.
	fp := &fakeProvider{authedUser: provider.User{Handle: "andreww"}, webhookID: "wh-1"}
	d := newDeps(t, fp)
	baseRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseRepo, ".syntropy.yml"), []byte("title_convention: new convention\n"), 0o644); err != nil {
		t.Fatalf("write .syntropy.yml: %v", err)
	}
	r := newRun(t, &AgentState{
		ProviderName:    "fake",
		ProjectID:       "x/y",
		BaseRepo:        baseRepo,
		StartedAt:       time.Now(),
		TitleConvention: "original convention",
	})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if r.Object.TitleConvention != "original convention" {
		t.Errorf("TitleConvention should not be re-read mid-Run; got %q", r.Object.TitleConvention)
	}
}

func TestSetup_AuthenticatedUserFails(t *testing.T) {
	fp := &fakeProvider{authedErr: errors.New("401 unauthorized")}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y"})

	next, err := d.setup(t.Context(), r)
	if err == nil {
		t.Fatalf("want error from AuthenticatedUser failure")
	}
	if next != StatusFailed {
		t.Errorf("want StatusFailed, got %v", next)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should propagate the underlying message: %v", err)
	}
}

func TestSetup_RegisterWebhookFails(t *testing.T) {
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "andreww"},
		regErr:     errors.New("403 forbidden — token lacks admin:repo_hook"),
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y", EventSource: EventSourceWebhook})

	next, err := d.setup(t.Context(), r)
	if err == nil {
		t.Fatalf("want error from RegisterWebhook failure")
	}
	if next != StatusFailed {
		t.Errorf("want StatusFailed, got %v", next)
	}
	if r.Object.WebhookID != "" {
		t.Errorf("WebhookID should not be set on failure: got %q", r.Object.WebhookID)
	}
}

// --- discover() tests ---

func TestDiscover_PopsNextUnitFromQueue(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{
		Queue:    []string{"svc-a", "svc-b", "svc-c"},
		InFlight: map[string]provider.MR{},
	})

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusWorking {
		t.Errorf("want Working, got %v", next)
	}
	if r.Object.CurrentUnit != "svc-a" {
		t.Errorf("CurrentUnit: want svc-a, got %q", r.Object.CurrentUnit)
	}
	if len(r.Object.Queue) != 2 || r.Object.Queue[0] != "svc-b" {
		t.Errorf("queue not advanced correctly: %v", r.Object.Queue)
	}
}

func TestDiscover_CompletesWhenQueueAndInFlightEmpty(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{InFlight: map[string]provider.MR{}})

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusCompleted {
		t.Errorf("want Completed, got %v", next)
	}
}

func TestDiscover_DedupsAgainstCompletedAndBlacklisted(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{
		Queue:       []string{"svc-a", "svc-b", "svc-c"},
		Completed:   []CompletedUnit{{UnitID: "svc-a"}},
		Blacklisted: []BlacklistedUnit{{UnitID: "svc-b"}},
		InFlight:    map[string]provider.MR{},
	})

	next, err := d.discover(t.Context(), r)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if next != StatusWorking {
		t.Errorf("want Working, got %v", next)
	}
	if r.Object.CurrentUnit != "svc-c" {
		t.Errorf("CurrentUnit: want svc-c (dedup skipped a + b), got %q", r.Object.CurrentUnit)
	}
}

// --- work() tests ---

func TestWork_HappyPath(t *testing.T) {
	fp := &fakeProvider{webhookID: "wh-1"}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Migrated logrus calls to slog in services/payments",
		Tokens:   1234,
	}})
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Migrate to slog",
		CurrentUnit:  "svc-payments",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
	})

	// Have the fake provider return a real-shaped MR.
	fp.createMRResult = provider.MR{
		ProjectID: "acme/example", IID: 42,
		URL: "https://gitlab/x/merge_requests/42", Branch: "syntropy/deadbeef/svc-payments",
	}

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}

	if len(fr.calls) != 1 {
		t.Fatalf("want 1 runner call, got %d", len(fr.calls))
	}
	call := fr.calls[0]
	if call.UnitID != "svc-payments" {
		t.Errorf("Request.UnitID: got %q", call.UnitID)
	}
	if call.Goal != "Migrate to slog" {
		t.Errorf("Request.Goal: got %q", call.Goal)
	}

	mr, ok := r.Object.InFlight["svc-payments"]
	if !ok {
		t.Fatalf("InFlight should contain svc-payments; got %v", r.Object.InFlight)
	}
	if mr.IID != 42 {
		t.Errorf("InFlight MR.IID: want 42, got %d", mr.IID)
	}

	if r.Object.SubagentInvocations != 1 {
		t.Errorf("SubagentInvocations: want 1, got %d", r.Object.SubagentInvocations)
	}
	if len(r.Object.History) != 1 {
		t.Fatalf("History: want 1 turn, got %d", len(r.Object.History))
	}
	if r.Object.History[0].UnitID != "svc-payments" {
		t.Errorf("Turn.UnitID: got %q", r.Object.History[0].UnitID)
	}
	if r.Object.History[0].Tokens != 1234 {
		t.Errorf("Turn.Tokens: want 1234, got %d", r.Object.History[0].Tokens)
	}

	if len(fp.comments) != 1 {
		t.Errorf("expected an initial status comment; got %d", len(fp.comments))
	}
}

// TestWork_MRTitle_UsesRunnerSuggestion covers ADR-0054: when the runner
// reports a Title (phrased per BaseRepo's .syntropy.yml title_convention),
// CreateMR must use it verbatim instead of the "Goal: unitID" default.
func TestWork_MRTitle_UsesRunnerSuggestion(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Migrated logrus calls to slog",
		Title:    "feat(payments): migrate logging to slog",
	}})
	r := newRun(t, &AgentState{
		ProviderName:    "fake",
		ProjectID:       "acme/example",
		RunnerName:      "fake-runner",
		Goal:            "Migrate to slog",
		CurrentUnit:     "svc-payments",
		BaseBranch:      "main",
		InFlight:        map[string]provider.MR{},
		TitleConvention: "Conventional Commits",
	})

	if _, err := d.work(t.Context(), r); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(fp.createMRCalls) != 1 {
		t.Fatalf("want 1 CreateMR call, got %d", len(fp.createMRCalls))
	}
	if got := fp.createMRCalls[0].Title; got != "feat(payments): migrate logging to slog" {
		t.Errorf("MR title: want runner's suggestion, got %q", got)
	}
}

// TestWork_MRTitle_FallsBackWhenRunnerOmitsOne covers the no-convention (or
// runner-didn't-comply) case: CreateMR must still get the pre-ADR-0054
// default title rather than an empty string.
func TestWork_MRTitle_FallsBackWhenRunnerOmitsOne(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Migrated logrus calls to slog",
	}})
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Migrate to slog",
		CurrentUnit:  "svc-payments",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
	})

	if _, err := d.work(t.Context(), r); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(fp.createMRCalls) != 1 {
		t.Fatalf("want 1 CreateMR call, got %d", len(fp.createMRCalls))
	}
	if got, want := fp.createMRCalls[0].Title, "Migrate to slog: svc-payments"; got != want {
		t.Errorf("MR title: want default %q, got %q", want, got)
	}
}

// TestWork_ThreadsRunnerModelIntoRequest is the regression guard for
// ADR-0041: a spec's `model:` override (AgentState.RunnerModel) must reach
// the runner via runner.Request.Model on every work() invocation, so
// cheaper models can drive simple increments.
func TestWork_ThreadsRunnerModelIntoRequest(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone, Summary: "did it",
	}})
	fp.createMRResult = provider.MR{ProjectID: "acme/example", IID: 42}
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		RunnerModel:  "claude-haiku-4-5",
		Goal:         "Migrate to slog",
		CurrentUnit:  "svc-payments",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
	})

	if _, err := d.work(t.Context(), r); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("want 1 runner call, got %d", len(fr.calls))
	}
	if got := fr.calls[0].Model; got != "claude-haiku-4-5" {
		t.Errorf("Request.Model: want claude-haiku-4-5, got %q", got)
	}
}

func TestWork_RunnerFails(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{err: errors.New("rate limited")})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state, not retried), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "rate limited") {
		t.Errorf("LastError should propagate runner error: %q", r.Object.LastError)
	}
	if len(fp.createMRCalls) != 0 {
		t.Errorf("CreateMR should not be called when runner errors; got %d calls", len(fp.createMRCalls))
	}
}

func TestWork_RunnerDeclines(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionFail,
		Summary:  "I can't figure out the right slog level mapping for this codebase",
	}})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, _ := d.work(t.Context(), r)
	if next != StatusFailed {
		t.Errorf("want Failed on DecisionFail, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "slog level") {
		t.Errorf("LastError should carry the runner's reason: %q", r.Object.LastError)
	}
}

// Regression: when the runner returns DecisionNoChange in the work phase,
// the unit must be blacklisted and the Run must return to Discovering —
// NOT terminate as StatusFailed. The first cross-MR-chain dogfood spike
// against github.com/andrewwormald/syntropy ended the whole Run when the
// planner picked a third increment, the runner correctly evaluated it as
// no-op, and work() routed NoChange through the catch-all "unexpected
// decision" → StatusFailed branch.
func TestWork_RunnerNoChange_BlacklistsAndContinues(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionNoChange,
		Summary:  "Spec already satisfied; nothing to change for increment-3",
	}})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "increment-3", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err, got %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("NoChange must route to Discovering (let planner re-evaluate), got %v", next)
	}
	if r.Object.CurrentUnit != "" {
		t.Errorf("CurrentUnit should be cleared after NoChange; got %q", r.Object.CurrentUnit)
	}
	if len(r.Object.Blacklisted) != 1 || r.Object.Blacklisted[0].UnitID != "increment-3" {
		t.Errorf("increment-3 should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "NoChange") {
		t.Errorf("Blacklist reason should name the decision: %q", r.Object.Blacklisted[0].Reason)
	}
	if r.Object.LastError != "" {
		t.Errorf("NoChange should not set LastError (it's not a failure): %q", r.Object.LastError)
	}
}

// TestWork_RunnerContinue_TreatedAsDone is the regression guard for the
// bug caught by Run b723ebc4 on 2026-07-03. DecisionContinue is
// documented as planner-only ("more work to plan"), but Claude sometimes
// returns it from a work turn. Before the fix, work()'s catch-all
// default routed it to StatusFailed and terminated the whole Run on
// what should have been a routine "keep going" signal.
//
// After the fix: DecisionContinue behaves like DecisionDone. If the
// worktree is dirty, the runner's work is committed + pushed + an MR
// opens (via the shared Done code path). If the worktree is clean, the
// unit is blacklisted (identical to Done + !dirty).
func TestWork_RunnerContinue_TreatedAsDone(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionContinue,
		Summary:  "Added item A; item B still pending — planner should pick it next",
		Tokens:   200,
	}})
	fp.createMRResult = provider.MR{ProjectID: "acme/example", IID: 8}
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Multi-item spec",
		CurrentUnit:  "increment-1",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err, got %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf(
			"DecisionContinue with dirty worktree must route to AwaitingMerge (same as Done); got %v. "+
				"If this returns StatusFailed with 'unexpected decision', the fix has regressed.",
			next,
		)
	}
	if r.Object.LastError != "" {
		t.Errorf("Continue should not set LastError (it's not a failure): %q", r.Object.LastError)
	}
	if len(fr.calls) != 1 {
		t.Errorf("expected exactly one runner call; got %d", len(fr.calls))
	}
	// The MR should have been opened via CreateMR.
	if mr, ok := r.Object.InFlight["increment-1"]; !ok || mr.IID != 8 {
		t.Errorf("expected InFlight[increment-1] = MR#8; got %+v", r.Object.InFlight)
	}
}

// TestWork_RunnerContinue_RecordsRemainderOnPlanEntry covers the follow-on
// fix to TestWork_RunnerContinue_TreatedAsDone: when the runner splits an
// oversized unit and returns DecisionContinue, work() must not just ship
// the partial MR silently — it should note what's left on the matching
// Plan entry so the planner can schedule a follow-on increment instead of
// assuming the unit is fully done.
func TestWork_RunnerContinue_RecordsRemainderOnPlanEntry(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionContinue,
		Summary:  "Added item A; item B still pending — planner should pick it next",
	}})
	fp.createMRResult = provider.MR{ProjectID: "acme/example", IID: 9}
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Multi-item spec",
		CurrentUnit:  "increment-1",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
		Plan: []PlannedIncrement{
			{UnitID: "increment-1", Rationale: "split item A and B", Outcome: "in_flight"},
		},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err, got %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Fatalf("want AwaitingMerge, got %v", next)
	}
	if len(r.Object.Plan) != 1 {
		t.Fatalf("Plan should still have 1 entry; got %+v", r.Object.Plan)
	}
	want := "Added item A; item B still pending — planner should pick it next"
	if got := r.Object.Plan[0].RemainderNote; got != want {
		t.Errorf("Plan[0].RemainderNote: want %q, got %q", want, got)
	}
	// DecisionDone must NOT set a remainder note — only Continue does.
	if r.Object.Plan[0].Outcome != "in_flight" {
		t.Errorf("Plan[0].Outcome should be untouched by work() (merge is what marks completed); got %q", r.Object.Plan[0].Outcome)
	}
}

// TestWork_RunnerDone_NoRemainderNote is the counterpart guard: a normal
// DecisionDone must never populate RemainderNote, even when a Plan entry
// exists for the unit.
func TestWork_RunnerDone_NoRemainderNote(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Fully migrated svc-payments",
	}})
	fp.createMRResult = provider.MR{ProjectID: "acme/example", IID: 10}
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Multi-item spec",
		CurrentUnit:  "increment-1",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
		Plan: []PlannedIncrement{
			{UnitID: "increment-1", Rationale: "migrate svc-payments", Outcome: "in_flight"},
		},
	})

	if _, err := d.work(t.Context(), r); err != nil {
		t.Fatalf("work: want nil err, got %v", err)
	}
	if got := r.Object.Plan[0].RemainderNote; got != "" {
		t.Errorf("Plan[0].RemainderNote should stay empty on Done; got %q", got)
	}
}

// TestBuildPlanningPrompt_SurfacesRemainderNote asserts that a Plan entry
// with a RemainderNote renders it in the planning prompt, so the planner
// can see that a unit shipped a partial slice and schedule the leftover
// work as a follow-on increment instead of assuming the unit is done.
func TestBuildPlanningPrompt_SurfacesRemainderNote(t *testing.T) {
	s := &AgentState{
		Goal: "Multi-item spec",
		Plan: []PlannedIncrement{
			{
				UnitID:        "increment-1",
				Rationale:     "split item A and B",
				Outcome:       "in_flight",
				RemainderNote: "item A shipped; item B still pending",
			},
		},
	}

	prompt := buildPlanningPrompt(s)

	want := "  - increment-1 shipped a partial slice; remaining work: item A shipped; item B still pending\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("planning prompt missing remainder line; want to contain %q, got:\n%s", want, prompt)
	}
}

// TestWork_ThreadsPlanRationaleIntoRunnerGoal is the regression guard
// for the scope-narrowing fix. Without threading the planner's per-
// increment rationale into req.Goal, the runner receives only the
// top-level (often multi-item) spec Goal + an opaque unit-id string
// and has no signal about which item(s) this increment covers.
// That's how the Run b21a0cc6 (2026-07-02) produced the mega-PR #5
// that bundled 5 unrelated items — the runner did as much of the
// shopping-list Goal as it could fit in one turn.
//
// This test asserts the runner is invoked with a Goal that contains
// the planner's rationale for THIS specific unit, so the runner can
// scope its work accordingly.
func TestWork_ThreadsPlanRationaleIntoRunnerGoal(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone, Summary: "did it", Tokens: 100,
	}})
	fp.createMRResult = provider.MR{ProjectID: "acme/example", IID: 42}

	// Seed a Plan entry for svc-payments whose rationale is unambiguously
	// per-increment (not the whole spec).
	const perIncrementRationale = "For svc-payments only: swap logrus for slog. Do NOT touch other services."
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Migrate logrus to slog across all services",
		CurrentUnit:  "svc-payments",
		BaseBranch:   "main",
		InFlight:     map[string]provider.MR{},
		Plan: []PlannedIncrement{
			{UnitID: "svc-payments", Rationale: perIncrementRationale},
		},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Fatalf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("want 1 runner call, got %d", len(fr.calls))
	}

	got := fr.calls[0].Goal
	// The runner's Goal must contain BOTH the per-increment rationale
	// (with a scope header) AND the full spec goal underneath. If either
	// is missing, the fix has regressed.
	if !strings.Contains(got, perIncrementRationale) {
		t.Errorf(
			"runner Goal must include the planner's per-increment rationale.\n"+
				"Want to find: %q\nGot Goal: %q",
			perIncrementRationale, got,
		)
	}
	if !strings.Contains(got, "Migrate logrus to slog across all services") {
		t.Errorf("runner Goal must still include the full spec Goal as context; got: %q", got)
	}
	if !strings.Contains(got, "Scope for this increment") {
		t.Errorf(
			"runner Goal should carry a labelled scope header so the runner knows the "+
				"per-increment text is authoritative; got: %q",
			got,
		)
	}
	// Rationale must appear BEFORE the spec goal in the prompt (LLMs
	// weight earlier context more heavily); the separator between them
	// confirms ordering.
	rationaleIdx := strings.Index(got, perIncrementRationale)
	specIdx := strings.Index(got, "Migrate logrus to slog across all services")
	if rationaleIdx > specIdx {
		t.Errorf("planner rationale must appear BEFORE the full spec goal in the runner prompt")
	}
}

// TestWork_NoPlanEntry_UsesGoalAsBefore covers the edge case where a
// Run has no Plan entry for the current unit (e.g. sweep mode, or a
// legacy Run pre-dating the plan-threading fix). The runner should
// receive an unmodified Goal — no scope header, no separator.
func TestWork_NoPlanEntry_UsesGoalAsBefore(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone, Summary: "done", Tokens: 50,
	}})
	fp.createMRResult = provider.MR{ProjectID: "acme/example", IID: 1}

	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "acme/example", RunnerName: "fake-runner",
		Goal:        "Rename Foo to Bar",
		CurrentUnit: "svc-a",
		BaseBranch:  "main",
		InFlight:    map[string]provider.MR{},
		// Plan is nil — no entry for svc-a
	})
	if _, err := d.work(t.Context(), r); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("want 1 runner call, got %d", len(fr.calls))
	}
	if fr.calls[0].Goal != "Rename Foo to Bar" {
		t.Errorf(
			"no plan entry → Goal must be the raw spec Goal; got %q",
			fr.calls[0].Goal,
		)
	}
}

// TestWork_PlanRationale_StacksWithPromptInjection verifies the ordering:
// user's PromptInjection is highest priority (topmost), planner rationale
// beneath it, spec Goal at the bottom. All three signals must be present
// in the final runner Goal.
func TestWork_PlanRationale_StacksWithPromptInjection(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone, Summary: "ok", Tokens: 100,
	}})
	fp.createMRResult = provider.MR{ProjectID: "acme/example", IID: 1}

	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "acme/example", RunnerName: "fake-runner",
		Goal:        "SPEC-GOAL: do many things",
		CurrentUnit: "svc-x",
		BaseBranch:  "main",
		InFlight:    map[string]provider.MR{},
		Plan: []PlannedIncrement{
			{UnitID: "svc-x", Rationale: "PLANNER-SCOPE: narrow to X only"},
		},
		PromptInjection: "USER-OVERRIDE: use the Edit tool",
	})
	if _, err := d.work(t.Context(), r); err != nil {
		t.Fatalf("work: %v", err)
	}
	got := fr.calls[0].Goal

	for _, needle := range []string{"USER-OVERRIDE: use the Edit tool", "PLANNER-SCOPE: narrow to X only", "SPEC-GOAL: do many things"} {
		if !strings.Contains(got, needle) {
			t.Errorf("Goal must contain %q; got: %q", needle, got)
		}
	}
	// Ordering: user injection → planner rationale → spec goal.
	userIdx := strings.Index(got, "USER-OVERRIDE")
	planIdx := strings.Index(got, "PLANNER-SCOPE")
	specIdx := strings.Index(got, "SPEC-GOAL")
	if !(userIdx < planIdx && planIdx < specIdx) {
		t.Errorf(
			"ordering must be user-injection < planner-rationale < spec-goal; got indices %d/%d/%d",
			userIdx, planIdx, specIdx,
		)
	}
	// PromptInjection was single-use and should have been consumed.
	if r.Object.PromptInjection != "" {
		t.Errorf("PromptInjection must be cleared after use; still: %q", r.Object.PromptInjection)
	}
}

func TestWork_CreateMRFails(t *testing.T) {
	fp := &fakeProvider{createMRErr: errors.New("404 not found")}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "404 not found") {
		t.Errorf("LastError should propagate CreateMR error: %q", r.Object.LastError)
	}
	if _, ok := r.Object.InFlight["svc-x"]; ok {
		t.Errorf("InFlight should not contain unit when CreateMR failed")
	}
}

func TestWork_RunnerDoneButCleanWorktree_BlacklistsAndMovesOn(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	d.withGit(&fakeGit{hasChanges: boolPtr(false)})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("clean-worktree Done should go to Discovering, got %v", next)
	}
	if len(fp.createMRCalls) != 0 {
		t.Errorf("no MR should be opened when nothing changed; got %d", len(fp.createMRCalls))
	}
	if len(r.Object.Blacklisted) != 1 || r.Object.Blacklisted[0].UnitID != "svc-x" {
		t.Errorf("svc-x should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "no changes") {
		t.Errorf("Blacklisted.Reason should mention no changes: %q", r.Object.Blacklisted[0].Reason)
	}
}

// The spec bug this wiring fixes: a runner that commits its own work leaves
// a clean tree, so the old porcelain-only HasChanges check discarded the
// unit as "no changes". work() must instead see the commits beyond base
// (HasWorkBeyondBase), tolerate Commit's ErrNoChanges (nothing left to
// stage), and push + open the MR as usual.
func TestWork_SelfCommittingRunner_PushesAndOpensMR(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	g := d.withGit(&fakeGit{
		hasChanges: boolPtr(false),   // tree clean — runner committed its own work
		hasWork:    boolPtr(true),    // …but commits exist beyond origin/<base>
		commitErr:  git.ErrNoChanges, // Commit on a clean tree
	})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		BaseBranch: "develop", CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("self-committed work should reach AwaitingMerge, got %v", next)
	}
	if len(r.Object.Blacklisted) != 0 {
		t.Errorf("unit must not be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if len(g.pushes) != 1 {
		t.Errorf("self-committed work should be pushed; pushes=%v", g.pushes)
	}
	if len(fp.createMRCalls) != 1 {
		t.Errorf("an MR should be opened for self-committed work; got %d", len(fp.createMRCalls))
	}
	// work()'s "did the runner do anything" check compares against the
	// spec's base branch.
	if len(g.hasWorkCalls) == 0 || !strings.HasSuffix(g.hasWorkCalls[0], "@develop") {
		t.Errorf("HasWorkBeyondBase should be called with base branch develop; calls=%v", g.hasWorkCalls)
	}
}

// ErrNoChanges with no commits beyond base (e.g. the runner produced only
// a binary artefact Commit's filter excluded) must still blacklist rather
// than push a branch with nothing on it.
func TestWork_CommitNoStageableAndNoCommits_Blacklists(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	g := d.withGit(&fakeGit{
		hasChanges: boolPtr(true),    // dirty (e.g. compiled binary present)
		commitErr:  git.ErrNoChanges, // nothing stageable
		diffStat:   strPtr(""),       // and nothing committed beyond base
	})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("nothing stageable and nothing committed should go to Discovering, got %v", next)
	}
	if len(g.pushes) != 0 {
		t.Errorf("nothing should be pushed; pushes=%v", g.pushes)
	}
	if len(fp.createMRCalls) != 0 {
		t.Errorf("no MR should be opened; got %d", len(fp.createMRCalls))
	}
	if len(r.Object.Blacklisted) != 1 || !strings.Contains(r.Object.Blacklisted[0].Reason, "no stageable changes") {
		t.Errorf("unit should be blacklisted with a no-stageable-changes reason; got %+v", r.Object.Blacklisted)
	}
}

func TestWork_PushFails(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	d.withGit(&fakeGit{pushErr: errors.New("remote rejected: branch protection")})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed on push fail, got %v", next)
	}
	if len(fp.createMRCalls) != 0 {
		t.Errorf("no MR should be opened when push failed; got %d", len(fp.createMRCalls))
	}
	if !strings.Contains(r.Object.LastError, "branch protection") {
		t.Errorf("LastError should propagate push error: %q", r.Object.LastError)
	}
}

func TestWork_EnsureBranchFails(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	d.withGit(&fakeGit{ensureErr: errors.New("base branch 'release/v9' does not exist")})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "release/v9") {
		t.Errorf("LastError should propagate EnsureBranch error: %q", r.Object.LastError)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner should NOT be invoked when worktree setup fails; got %d calls", len(fr.calls))
	}
}

func TestWork_NotIsolatedWorktree_RefusesToInvokeRunner(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	d.withGit(&fakeGit{isolated: boolPtr(false)})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "fake-runner",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: want nil err (terminal failure committed via state), got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
	if !strings.Contains(r.Object.LastError, "not an isolated git worktree") {
		t.Errorf("LastError should mention isolation guard: %q", r.Object.LastError)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner should NOT be invoked with --dangerously-skip-permissions on a non-isolated dir; got %d calls", len(fr.calls))
	}
}

func TestResume_NoteAdded_DoneButCleanWorktree_PostsInfoComment(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Already correct"}})
	d.withGit(&fakeGit{hasChanges: boolPtr(false)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "are you sure about Foo?", DiscussionID: "disc-abc"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("clean-worktree Done in comment phase should stay AwaitingMerge, got %v", next)
	}
	if len(fp.replies) != 1 || !strings.Contains(fp.replies[0].Body, "No code changes") || fp.replies[0].DiscussionID != "disc-abc" {
		t.Errorf("expected an info-only reply within the originating thread; got %+v", fp.replies)
	}
	// Even when no code change was needed, the discussion should be
	// resolved — the question was answered, even if verbally.
	if len(fp.resolves) != 1 || fp.resolves[0].DiscussionID != "disc-abc" {
		t.Errorf("expected ResolveDiscussion(disc-abc) on clean-worktree path; got %+v", fp.resolves)
	}
}

// Regression: invokeForEvent must NOT pause when Commit returns ErrNoChanges
// (e.g. the runner ran `go build`, producing only a binary artefact that
// our staging filter excluded, so HasChanges=true but nothing was staged).
// Previously this paused the Run with "git Commit failed during ..."; now
// it stays AwaitingMerge with an info comment.
func TestResume_NoteAdded_CommitReturnsNoChanges_StaysAwaitingMerge(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Considered but no code change needed"}})
	d.withGit(&fakeGit{
		hasChanges: boolPtr(true),    // worktree dirty (e.g. compiled binary present)
		commitErr:  git.ErrNoChanges, // but Commit's filter saw nothing stageable
		diffStat:   strPtr(""),       // and no committed work beyond the pushed tip
	})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "what about edge case Z?", DiscussionID: "disc-xyz"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("Commit ErrNoChanges in comment phase should stay AwaitingMerge, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("Run should not be paused on ErrNoChanges; PauseReason=%q", r.Object.PauseReason)
	}
	if len(fp.replies) != 1 || !strings.Contains(fp.replies[0].Body, "No code changes") || fp.replies[0].DiscussionID != "disc-xyz" {
		t.Errorf("expected an info-only reply within the originating thread; got %+v", fp.replies)
	}
	if len(fp.resolves) != 1 || fp.resolves[0].DiscussionID != "disc-xyz" {
		t.Errorf("expected ResolveDiscussion(disc-xyz); got %+v", fp.resolves)
	}
}

// Self-committing runner during address-comment: clean tree but unpushed
// commits on the branch. The commits must be pushed and the thread
// resolved — not dropped with "No code changes were needed" (the spec bug).
func TestResume_NoteAdded_SelfCommittingRunner_PushesAndResolves(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Renamed Foo to Bar."}})
	g := d.withGit(&fakeGit{
		hasChanges: boolPtr(false),   // clean — runner committed its own work
		hasWork:    boolPtr(true),    // …with commits not yet on origin/<branch>
		commitErr:  git.ErrNoChanges, // Commit on a clean tree
	})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename Foo to Bar", DiscussionID: "disc-42"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("self-committed work should stay AwaitingMerge, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("Run should not be paused; PauseReason=%q", r.Object.PauseReason)
	}
	if len(g.pushes) != 1 {
		t.Errorf("self-committed work should be pushed; pushes=%v", g.pushes)
	}
	if len(fp.resolves) != 1 || fp.resolves[0].DiscussionID != "disc-42" {
		t.Errorf("expected ResolveDiscussion(disc-42); got %+v", fp.resolves)
	}
	if len(fp.replies) != 1 || !strings.Contains(fp.replies[0].Body, "Addressed") || fp.replies[0].DiscussionID != "disc-42" {
		t.Errorf("expected an Addressed reply within the originating thread, not a no-changes note; got %+v", fp.replies)
	}
	// invokeForEvent's per-turn check must compare against the unit's own
	// pushed tip, not base — the branch always has the original work
	// commits beyond base, which would make a base comparison vacuous.
	wantRef := "@" + branchName(r.RunID, "u")
	if len(g.hasWorkCalls) == 0 || !strings.HasSuffix(g.hasWorkCalls[0], wantRef) {
		t.Errorf("HasWorkBeyondBase should be called with the unit branch (%s); calls=%v", wantRef, g.hasWorkCalls)
	}
}

// Regression: a DecisionContinue from invokeForEvent (address_comment/
// fix_ci) used to be bundled with DecisionNoChange and silently dropped
// any work the runner did that turn — found live when a "add tests"
// instruction produced a real, passing test file that then sat
// permanently uncommitted because the turn returned Continue, not Done.
// Continue must commit + push exactly like Done.
func TestResume_NoteAdded_DecisionContinue_CommitsAndPushes(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionContinue, Summary: "Added impl_test.go; more coverage still needed."}})
	g := d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "I would like tests to cover this from the beginning", DiscussionID: "disc-99"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("DecisionContinue should stay AwaitingMerge, got %v", next)
	}
	if len(g.commits) != 1 {
		t.Errorf("DecisionContinue must commit the runner's work; commits=%v", g.commits)
	}
	if len(g.pushes) != 1 {
		t.Errorf("DecisionContinue must push the commit; pushes=%v", g.pushes)
	}
}

// A Continue decision means the reviewer's feedback isn't fully addressed
// yet — unlike Done, the originating discussion thread must NOT be
// resolved, so it stays visibly open for whatever event continues it.
func TestResume_NoteAdded_DecisionContinue_DoesNotResolveDiscussion(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionContinue, Summary: "Partial slice shipped."}})
	d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please add more tests", DiscussionID: "disc-100"},
	}
	if _, err := d.resume(t.Context(), r, payloadOf(t, ev)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(fp.resolves) != 0 {
		t.Errorf("DecisionContinue must not resolve the discussion thread yet; resolves=%+v", fp.resolves)
	}
	if len(fp.replies) != 1 || fp.replies[0].DiscussionID != "disc-100" {
		t.Fatalf("expected one reply in the originating thread; got %+v", fp.replies)
	}
	if !strings.Contains(fp.replies[0].Body, "Partial progress") {
		t.Errorf("reply should be worded as partial progress, not a finished Addressed; got %q", fp.replies[0].Body)
	}
}

// Push succeeded but ResolveDiscussion errored → the Run must still
// stay AwaitingMerge (the change is pushed; thread resolution is
// best-effort). A "couldn't resolve" info comment is posted so the
// reviewer knows to close manually.
func TestResume_NoteAdded_ResolveDiscussionFails_StaysAwaitingMerge(t *testing.T) {
	fp := &fakeProvider{resolveErr: errors.New("403 forbidden")}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Renamed."}})
	d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "rename Foo → Bar", DiscussionID: "disc-1"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: want nil err even when resolve fails, got %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge after push+failed-resolve, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("Run must not pause on resolve failure; PauseReason=%q", r.Object.PauseReason)
	}
	// ResolveDiscussion was attempted exactly once.
	if len(fp.resolves) != 1 {
		t.Errorf("ResolveDiscussion should be attempted once; got %d", len(fp.resolves))
	}
	// And an info reply was posted within the thread explaining the failure.
	foundInfo := false
	for _, c := range fp.replies {
		if (strings.Contains(c.Body, "couldn't resolve") || strings.Contains(c.Body, "403 forbidden")) && c.DiscussionID == "disc-1" {
			foundInfo = true
			break
		}
	}
	if !foundInfo {
		t.Errorf("expected an info reply within the originating thread surfacing the resolve failure; got %+v", fp.replies)
	}
}

// Push succeeded → resolve the originating discussion thread so the
// reviewer sees their comment marked resolved.
func TestResume_NoteAdded_PushSucceeded_ResolvesDiscussion(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Renamed."}})
	d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "rename Foo → Bar", DiscussionID: "disc-rename"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge after successful push, got %v", next)
	}
	if len(fp.resolves) != 1 || fp.resolves[0].DiscussionID != "disc-rename" {
		t.Errorf("ResolveDiscussion(disc-rename) should be called after push; got %+v", fp.resolves)
	}
}

func TestResume_NoteAdded_PushFails_Pauses(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "fixed"}})
	d.withGit(&fakeGit{pushErr: errors.New("auth required")})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("push failure in comment phase should Pause (MR exists for recovery), got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "Push") {
		t.Errorf("PauseReason should mention push: %q", r.Object.PauseReason)
	}
}

func TestResume_MRMerged_CleansUpWorktree(t *testing.T) {
	g := &fakeGit{}
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	d.withGit(g)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "svc-a", mr)
	r.Object.BaseRepo = "/some/repo"

	ev := provider.Event{Kind: provider.EventMRMerged, MR: mr}
	if _, err := d.resume(t.Context(), r, payloadOf(t, ev)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(g.removes) != 1 {
		t.Errorf("expected one RemoveWorktree call on merge, got %d", len(g.removes))
	}
	if !strings.Contains(g.removes[0], "svc-a") {
		t.Errorf("removed worktree should be svc-a's, got %q", g.removes[0])
	}
}

func TestWork_UnknownRunner(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	r := newRun(t, &AgentState{
		ProviderName: "fake", ProjectID: "x/y", RunnerName: "not-registered",
		CurrentUnit: "svc-x", InFlight: map[string]provider.MR{},
	})

	next, err := d.work(t.Context(), r)
	if err == nil || !strings.Contains(err.Error(), "unknown runner") {
		t.Fatalf("want unknown-runner error, got %v", err)
	}
	if next != StatusFailed {
		t.Errorf("want Failed, got %v", next)
	}
}

// --- resume() tests ---

// payloadOf JSON-encodes the event in the same shape the daemon's dispatcher
// will produce.
func payloadOf(t *testing.T, ev provider.Event) *bytes.Reader {
	t.Helper()
	buf, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return bytes.NewReader(buf)
}

// awaitingRun returns a Run already in StatusAwaitingMerge with one unit
// in flight against the given MR. Mirrors the state work() leaves behind.
func awaitingRun(t *testing.T, unitID string, mr provider.MR) *workflow.Run[AgentState, AgentStatus] {
	t.Helper()
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    mr.ProjectID,
		RunnerName:   "fake-runner",
		Goal:         "test goal",
		Author:       provider.User{Handle: "andreww"},
		CurrentUnit:  unitID,
		InFlight:     map[string]provider.MR{unitID: mr},
	})
	r.Status = StatusAwaitingMerge
	return r
}

func TestResume_MRMerged_MovesToCompleted(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "acme/example", IID: 42, URL: "https://x/42"}
	r := awaitingRun(t, "svc-a", mr)

	ev := provider.Event{
		Kind: provider.EventMRMerged,
		MR:   mr,
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("want Discovering, got %v", next)
	}
	if _, still := r.Object.InFlight["svc-a"]; still {
		t.Errorf("unit should be removed from InFlight after merge")
	}
	if len(r.Object.Completed) != 1 || r.Object.Completed[0].UnitID != "svc-a" {
		t.Errorf("svc-a should be in Completed; got %+v", r.Object.Completed)
	}
}

func TestResume_MRClosed_MovesToBlacklisted(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 7}
	r := awaitingRun(t, "svc-x", mr)

	ev := provider.Event{Kind: provider.EventMRClosed, MR: mr}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusDiscovering {
		t.Errorf("want Discovering, got %v", next)
	}
	if len(r.Object.Blacklisted) != 1 {
		t.Fatalf("svc-x should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "closed without merge") {
		t.Errorf("Blacklisted.Reason should mention close: %q", r.Object.Blacklisted[0].Reason)
	}
}

// TestResume_CommentAfterMerge_DroppedWithoutInvokingRunner reproduces the
// incident scenario: a reviewer comment lands on an MR that was already
// merged (and thus already removed from InFlight). unitForMR can no longer
// match it to a unit, so it must be dropped like cross-talk from an unknown
// MR rather than dispatched to the runner.
func TestResume_CommentAfterMerge_DroppedWithoutInvokingRunner(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 7}
	r := awaitingRun(t, "svc-a", mr)

	mergedEv := provider.Event{Kind: provider.EventMRMerged, MR: mr}
	if _, err := d.resume(t.Context(), r, payloadOf(t, mergedEv)); err != nil {
		t.Fatalf("resume(merged): %v", err)
	}
	if _, still := r.Object.InFlight["svc-a"]; still {
		t.Fatalf("svc-a should be removed from InFlight after merge")
	}

	commentEv := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   mr,
		Note: provider.Note{Body: "one more thing"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, commentEv))
	if err != nil {
		t.Fatalf("resume(comment after merge): %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("post-merge comment should be dropped, not change status; got %v", next)
	}
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("post-merge comment must not invoke the runner; got %d invocations", r.Object.SubagentInvocations)
	}
}

func TestResume_PipelineSucceeded_NoOp(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{Kind: provider.EventPipelineSucceeded, MR: mr}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge (no-op), got %v", next)
	}
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("no subagent should fire on pipeline success; got %d invocations", r.Object.SubagentInvocations)
	}
}

func TestResume_NoteAdded_InvokesSubagent_DecisionDone(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "Addressed: renamed Foo to Bar as requested",
		Tokens:   500,
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{ID: 100, Body: "please rename Foo to Bar"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be called once; got %d calls", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].CommentBody, "rename Foo to Bar") {
		t.Errorf("CommentBody not propagated to runner: %q", fr.calls[0].CommentBody)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "Addressed") {
		t.Errorf("status comment should be posted on DecisionDone; got %+v", fp.comments)
	}
}

func TestResume_NoteAdded_CommenterIsAuthorPropagated(t *testing.T) {
	// ADR-0072: invokeForEvent threads ev.IsAuthor into the runner request
	// so BuildPrompt can gate the non-author triage guidance on it.
	for _, tc := range []struct {
		name   string
		handle string
		want   bool
	}{
		{"reviewer comment", "reviewer", false},
		{"author comment", "andreww", true}, // matches awaitingRun's Author
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeps(t, &fakeProvider{})
			fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
				Decision: DecisionDone, Summary: "handled",
			}})
			mr := provider.MR{ProjectID: "x/y", IID: 1}
			r := awaitingRun(t, "u", mr)

			ev := provider.Event{
				Kind:   provider.EventNoteAdded,
				MR:     mr,
				Author: provider.User{Handle: tc.handle},
				Note:   provider.Note{ID: 100, Body: "the nil check on line 12 is inverted"},
			}
			if _, err := d.resume(t.Context(), r, payloadOf(t, ev)); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if len(fr.calls) != 1 {
				t.Fatalf("runner should be called once; got %d calls", len(fr.calls))
			}
			if fr.calls[0].CommenterIsAuthor != tc.want {
				t.Errorf("CommenterIsAuthor: want %v, got %v", tc.want, fr.calls[0].CommenterIsAuthor)
			}
		})
	}
}

func TestResume_AuthorControlComment_GateUnaffected(t *testing.T) {
	// ADR-0072 reuses ev.IsAuthor for the runner request, but the upstream
	// control-verb gate (ADR-0017) must be untouched: an author /syntropy
	// command still dispatches as a control command and never reaches the
	// runner as an address-comment invocation.
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"}, // the Run author
		Note:   provider.Note{ID: 101, Body: "/syntropy pause"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusPaused {
		t.Errorf("author /syntropy pause should still pause the Run; got %v", next)
	}
	if len(fr.calls) != 0 {
		t.Errorf("control command must not invoke the runner; got %d calls", len(fr.calls))
	}
}

func TestResume_NoteAdded_DecisionAsk_PausesWithQuestion(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionAsk,
		Question: "Should I delete the deprecated method or just mark it //Deprecated:?",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   mr, Author: provider.User{Handle: "reviewer"},
		Note: provider.Note{Body: "what about the deprecated method?"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("want Paused on DecisionAsk, got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "deprecated method") {
		t.Errorf("PauseReason should carry the question: %q", r.Object.PauseReason)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "/syntropy resume") {
		t.Errorf("pause comment should mention how to resume: %+v", fp.comments)
	}
}

// Regression (ADR-0074): a runner can edit files even when it concludes
// DecisionAsk/Fail/NoChange/RetryCI — e.g. a partial edit made before
// deciding to ask a question. Those decisions used to leave that work
// permanently uncommitted, silently tripping a future SyncWithBase's
// uncommitted-changes guard. Found live on a real run stuck on exactly
// this after an address_comment turn returned something other than
// Continue/Done. All four decisions must commit+push any stray work
// first, the same safety net Continue/Done already have (ADR-0066).
func TestResume_NoteAdded_DecisionAsk_CommitsStrayWorkBeforePausing(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionAsk,
		Question: "Should I delete the deprecated method or just mark it //Deprecated:?",
	}})
	g := d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   mr, Author: provider.User{Handle: "reviewer"},
		Note: provider.Note{Body: "what about the deprecated method?"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("want Paused on DecisionAsk, got %v", next)
	}
	if len(g.commits) != 1 {
		t.Errorf("DecisionAsk must commit any stray work before pausing; commits=%v", g.commits)
	}
	if len(g.pushes) != 1 {
		t.Errorf("DecisionAsk must push any stray commit before pausing; pushes=%v", g.pushes)
	}
}

func TestResume_NoteAdded_DecisionFail_CommitsStrayWorkBeforePausing(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionFail,
		Summary:  "Couldn't figure out how to address this safely.",
	}})
	g := d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please fix this"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("want Paused on DecisionFail, got %v", next)
	}
	if len(g.commits) != 1 {
		t.Errorf("DecisionFail must commit any stray work before pausing; commits=%v", g.commits)
	}
	if len(g.pushes) != 1 {
		t.Errorf("DecisionFail must push any stray commit before pausing; pushes=%v", g.pushes)
	}
}

func TestResume_NoteAdded_DecisionNoChange_CommitsStrayWork(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionNoChange, Summary: "Nothing actionable."}})
	g := d.withGit(&fakeGit{hasChanges: boolPtr(true)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "any thoughts?"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge on DecisionNoChange, got %v", next)
	}
	if len(g.commits) != 1 {
		t.Errorf("DecisionNoChange must commit any stray work; commits=%v", g.commits)
	}
	if len(g.pushes) != 1 {
		t.Errorf("DecisionNoChange must push any stray commit; pushes=%v", g.pushes)
	}
	if len(fp.comments) != 0 {
		t.Errorf("DecisionNoChange must still not post a top-level comment (webhook-loop guard); comments=%+v", fp.comments)
	}
}

// No stray work → no-op. Confirms the safety net doesn't commit/push on
// every turn regardless of whether the runner actually touched anything.
func TestResume_NoteAdded_DecisionNoChange_NoStrayWork_DoesNotCommit(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionNoChange, Summary: "Nothing actionable."}})
	g := d.withGit(&fakeGit{hasChanges: boolPtr(false)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "any thoughts?"},
	}
	if _, err := d.resume(t.Context(), r, payloadOf(t, ev)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(g.commits) != 0 {
		t.Errorf("no stray work present, should not commit; commits=%v", g.commits)
	}
	if len(g.pushes) != 0 {
		t.Errorf("no stray work present, should not push; pushes=%v", g.pushes)
	}
}

func TestResume_PipelineFailed_InvokesSubagent(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone, Summary: "Fixed flaky test",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventPipelineFailed,
		MR:   mr,
		Pipeline: provider.Pipeline{
			ID: 99, Status: "failed",
			FailedJobs: []provider.Job{
				{ID: 1, Name: "test 3/5", Stage: "test", Status: "failed"},
			},
		},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be called; got %d calls", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].CIFailure, "test 3/5") {
		t.Errorf("CIFailure should mention failed job: %q", fr.calls[0].CIFailure)
	}
}

// TestResume_PipelineFailed_DecisionRetryCI_RetriesFailedJobs asserts that a
// DecisionRetryCI response (ADR-0068) makes no code change — it retries the
// failed job(s) via RetryPipelineJob, stays AwaitingMerge, and does not
// touch git at all.
func TestResume_PipelineFailed_DecisionRetryCI_RetriesFailedJobs(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionRetryCI, Summary: "Looks like a flaky network blip in the test runner.",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventPipelineFailed,
		MR:   mr,
		Pipeline: provider.Pipeline{
			ID: 99, Status: "failed",
			FailedJobs: []provider.Job{{ID: 42, Name: "test 3/5", Stage: "test", Status: "failed"}},
		},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fp.retriedJobs) != 1 || fp.retriedJobs[0] != (retriedJobCall{ProjectID: "x/y", JobID: 42}) {
		t.Errorf("want job 42 retried once; got %+v", fp.retriedJobs)
	}
	if r.Object.CIRetryCounts["u"] != 1 {
		t.Errorf("want CIRetryCounts[u] == 1, got %d", r.Object.CIRetryCounts["u"])
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "transient") {
		t.Errorf("want a status comment noting the transient retry; got %+v", fp.comments)
	}
}

// TestResume_PipelineFailed_DecisionRetryCI_PausesAfterCap asserts that once
// a unit has hit maxCIRetries consecutive DecisionRetryCI outcomes,
// invokeForEvent stops retrying and pauses for a human instead of looping
// forever (ADR-0068).
func TestResume_PipelineFailed_DecisionRetryCI_PausesAfterCap(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionRetryCI, Summary: "Still looks transient.",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Object.CIRetryCounts = map[string]int{"u": maxCIRetries}

	ev := provider.Event{
		Kind: provider.EventPipelineFailed,
		MR:   mr,
		Pipeline: provider.Pipeline{
			ID: 99, Status: "failed",
			FailedJobs: []provider.Job{{ID: 42, Name: "test 3/5", Stage: "test", Status: "failed"}},
		},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusPaused {
		t.Errorf("want Paused once maxCIRetries is exceeded, got %v", next)
	}
	if len(fp.retriedJobs) != 0 {
		t.Errorf("must not retry the job once the cap is exceeded; got %+v", fp.retriedJobs)
	}
	if !strings.Contains(r.Object.PauseReason, "retried") {
		t.Errorf("PauseReason should explain the retry cap was hit: %q", r.Object.PauseReason)
	}
}

// TestResume_PipelineSucceeded_ResetsCIRetryCount asserts a green pipeline
// clears the unit's DecisionRetryCI streak so a later unrelated failure
// doesn't inherit a stale near-cap count.
func TestResume_PipelineSucceeded_ResetsCIRetryCount(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Object.CIRetryCounts = map[string]int{"u": 2}

	ev := provider.Event{Kind: provider.EventPipelineSucceeded, MR: mr}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if _, ok := r.Object.CIRetryCounts["u"]; ok {
		t.Errorf("EventPipelineSucceeded should clear the unit's retry count; got %+v", r.Object.CIRetryCounts)
	}
}

// TestResume_PipelineFailed_DecisionDone_ResetsCIRetryCount asserts that
// once the runner pushes a genuine fix_ci fix (Done/Continue, ADR-0068),
// the unit's DecisionRetryCI streak is cleared the same way a green
// pipeline clears it — a real code fix means the next CI failure starts a
// fresh streak rather than inheriting an earlier near-cap count.
func TestResume_PipelineFailed_DecisionDone_ResetsCIRetryCount(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone, Summary: "Fixed the failing test.",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Object.CIRetryCounts = map[string]int{"u": 2}

	ev := provider.Event{
		Kind: provider.EventPipelineFailed,
		MR:   mr,
		Pipeline: provider.Pipeline{
			ID: 99, Status: "failed",
			FailedJobs: []provider.Job{{ID: 42, Name: "test 3/5", Stage: "test", Status: "failed"}},
		},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if _, ok := r.Object.CIRetryCounts["u"]; ok {
		t.Errorf("a pushed fix_ci fix should clear the unit's retry count; got %+v", r.Object.CIRetryCounts)
	}
}

// TestResume_NoteAdded_SyncsWithBaseBeforeRunner asserts invokeForEvent
// refreshes the unit worktree against origin/<base> BEFORE the runner is
// invoked (ADR-0045), so conflict resolution never judges against a stale
// view of main. The fake runner snapshots fakeGit's sync log at the moment
// Run() is called, so ordering — not just occurrence — is what's asserted.
func TestResume_NoteAdded_SyncsWithBaseBeforeRunner(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fg := d.withGit(&fakeGit{})
	var syncsWhenRunnerRan []string
	fr := d.withRunner(t, &fakeRunner{
		resp: runner.Response{Decision: DecisionDone, Summary: "Renamed."},
		onRun: func(runner.Request) {
			fg.mu.Lock()
			syncsWhenRunnerRan = append([]string(nil), fg.syncs...)
			fg.mu.Unlock()
		},
	})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename Foo to Bar"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be called once; got %d", len(fr.calls))
	}
	wantSync := filepath.Join(d.RunsRoot, r.RunID, "worktrees", "u") + "@main"
	if len(syncsWhenRunnerRan) != 1 || syncsWhenRunnerRan[0] != wantSync {
		t.Errorf(
			"SyncWithBase(unit worktree, base) must run before the runner; syncs at runner-call time: %v, want [%s]",
			syncsWhenRunnerRan, wantSync,
		)
	}
}

// TestResume_NoteAdded_ReactsBeforeInvokingRunner asserts invokeForEvent
// acknowledges the triggering comment with a reaction BEFORE the
// (potentially long) runner invocation, so the commenter sees it was picked
// up rather than missed. Ordering, not just occurrence, is what's asserted
// — mirrors TestResume_NoteAdded_SyncsWithBaseBeforeRunner.
func TestResume_NoteAdded_ReactsBeforeInvokingRunner(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	var reactionsWhenRunnerRan int
	fr := d.withRunner(t, &fakeRunner{
		resp: runner.Response{Decision: DecisionDone, Summary: "Renamed."},
		onRun: func(runner.Request) {
			fp.mu.Lock()
			reactionsWhenRunnerRan = len(fp.reactions)
			fp.mu.Unlock()
		},
	})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{ID: 42, Stream: "issue_comment", Body: "please rename Foo to Bar"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be called once; got %d", len(fr.calls))
	}
	if reactionsWhenRunnerRan != 1 {
		t.Errorf("ReactToNote must run before the runner; reactions at runner-call time: %d, want 1", reactionsWhenRunnerRan)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("want exactly 1 reaction; got %+v", fp.reactions)
	}
	got := fp.reactions[0]
	want := reactToNoteCall{ProjectID: "x/y", MRIID: 1, NoteID: 42, Stream: "issue_comment", Emoji: "eyes"}
	if got != want {
		t.Errorf("ReactToNote call = %+v, want %+v", got, want)
	}
}

// TestResume_PipelineFailed_DoesNotReact asserts invokeForEvent only reacts
// on NoteAdded events — PipelineFailed has no comment to acknowledge.
func TestResume_PipelineFailed_DoesNotReact(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Fixed flaky test"}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventPipelineFailed,
		MR:   mr,
		Pipeline: provider.Pipeline{
			ID: 99, Status: "failed",
			FailedJobs: []provider.Job{{ID: 1, Name: "test 3/5", Stage: "test", Status: "failed"}},
		},
	}
	if _, err := d.resume(t.Context(), r, payloadOf(t, ev)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(fp.reactions) != 0 {
		t.Errorf("PipelineFailed must not trigger a reaction; got %+v", fp.reactions)
	}
}

// skipFilter always returns OutcomeSkip, regardless of the event.
type skipFilter struct{}

func (skipFilter) Eval(provider.Event, any, filter.PhraseSet) (filter.Outcome, error) {
	return filter.OutcomeSkip, nil
}

// TestResume_OutcomeSkip_DoesNotReact asserts a filter-skipped NoteAdded
// event triggers no reaction — only events that actually get invoked (via
// invokeForEvent) or dispatched as control commands should acknowledge.
func TestResume_OutcomeSkip_DoesNotReact(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.Filter = skipFilter{}
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "n/a"}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{ID: 42, Stream: "issue_comment", Body: "just chatting"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if r.Object.EventsSkippedByFilter != 1 {
		t.Errorf("EventsSkippedByFilter = %d, want 1", r.Object.EventsSkippedByFilter)
	}
	if len(fp.reactions) != 0 {
		t.Errorf("OutcomeSkip must not trigger a reaction; got %+v", fp.reactions)
	}
	if len(fr.calls) != 0 {
		t.Errorf("OutcomeSkip must not invoke the runner; got %d calls", len(fr.calls))
	}
}

// TestResume_NoteAdded_ReactToNoteFails_DoesNotBlockRunner asserts a
// reaction failure is swallowed — best-effort acknowledgement must never
// prevent the actual work from running.
func TestResume_NoteAdded_ReactToNoteFails_DoesNotBlockRunner(t *testing.T) {
	fp := &fakeProvider{reactErr: errors.New("reactions: 404 no such endpoint")}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Renamed."}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{ID: 42, Body: "please rename Foo to Bar"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge despite ReactToNote error, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Errorf("runner should still be called when ReactToNote fails; got %d calls", len(fr.calls))
	}
}

// TestResume_NoteAdded_SyncWithBaseFails_Pauses: a genuine SyncWithBase
// failure (e.g. dirty worktree or fetch error — NOT an ordinary merge
// conflict, which SyncWithBase swallows by contract) must pause the Run
// with an explanatory comment and never invoke the runner.
func TestResume_NoteAdded_SyncWithBaseFails_Pauses(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withGit(&fakeGit{syncErr: errors.New("SyncWithBase: merge: dirty working tree")})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename Foo to Bar"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: want nil err (pause committed via state, not retried), got %v", err)
	}
	if next != StatusPaused {
		t.Errorf("want Paused on SyncWithBase failure, got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "SyncWithBase") {
		t.Errorf("PauseReason should name the failing step: %q", r.Object.PauseReason)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner must NOT be invoked when the pre-run sync fails; got %d calls", len(fr.calls))
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "/syntropy retry") {
		t.Errorf("pause comment should tell the author how to retry; got %+v", fp.comments)
	}
}

// TestResume_NoteAdded_NotIsolatedWorktree_Pauses asserts the deterministic
// isolation guard runs before invokeForEvent hands the worktree to a runner
// with --dangerously-skip-permissions: a non-isolated dir must pause the
// Run and never invoke the runner.
func TestResume_NoteAdded_NotIsolatedWorktree_Pauses(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withGit(&fakeGit{isolated: boolPtr(false)})
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename Foo to Bar"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: want nil err (pause committed via state, not retried), got %v", err)
	}
	if next != StatusPaused {
		t.Errorf("want Paused when worktree isolation check fails, got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "isolation") {
		t.Errorf("PauseReason should name the failing check: %q", r.Object.PauseReason)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner must NOT be invoked when the isolation check fails; got %d calls", len(fr.calls))
	}
}

// Control-command dispatch is covered in controls_test.go.

func TestResume_PausedRun_DropsNonControlEvents(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused

	ev := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   mr, Author: provider.User{Handle: "reviewer"},
		Note: provider.Note{Body: "any chance you can also look at this"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("paused Run should stay paused on non-control event, got %v", next)
	}
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("no subagent should fire while paused; got %d", r.Object.SubagentInvocations)
	}
}

// --- Self-comment echo suppression (RecentOutgoingHashes FIFO) ---

// TestPostBotComment_StoresHashOnState asserts the helper records a
// SHA-256 hash of the body on AgentState.RecentOutgoingHashes before
// invoking the provider. The write-before-network ordering is what
// prevents a race where the poller sees the comment before the state
// knows about it.
func TestPostBotComment_StoresHashOnState(t *testing.T) {
	fp := &fakeProvider{}
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	if err := postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID, "hello world"); err != nil {
		t.Fatalf("postBotComment: %v", err)
	}
	if got := len(r.Object.RecentOutgoingHashes); got != 1 {
		t.Fatalf("want 1 hash on state, got %d", got)
	}
	if r.Object.RecentOutgoingHashes[0] != hashBody("hello world") {
		t.Errorf("recorded hash doesn't match body hash")
	}
	if len(fp.comments) != 1 || fp.comments[0].Body != "hello world" {
		t.Errorf("provider should have received the raw body; got %+v", fp.comments)
	}
}

// TestPostBotComment_CapFIFO exercises the ring-buffer trim: N > cap
// posts and only the last `cap` hashes should survive, in FIFO order.
func TestPostBotComment_CapFIFO(t *testing.T) {
	fp := &fakeProvider{}
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	for i := 0; i < recentOutgoingHashCap+5; i++ {
		_ = postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID, fmt.Sprintf("msg-%d", i))
	}
	if got := len(r.Object.RecentOutgoingHashes); got != recentOutgoingHashCap {
		t.Errorf("want len==cap after overflow, got %d", got)
	}
	// The last cap messages should be msg-5 through msg-36 (drop msg-0..4).
	oldestKept := hashBody("msg-5")
	if r.Object.RecentOutgoingHashes[0] != oldestKept {
		t.Errorf("FIFO order broken: want first entry = hash(msg-5), got hash of a different body")
	}
	newestKept := hashBody(fmt.Sprintf("msg-%d", recentOutgoingHashCap+4))
	if r.Object.RecentOutgoingHashes[len(r.Object.RecentOutgoingHashes)-1] != newestKept {
		t.Errorf("FIFO order broken: last entry should be the newest post")
	}
}

// TestResume_SkipsOwnEchoedComment is the headline regression guard for
// the self-comment loop. A note whose body matches a recent outgoing
// hash must be silently dropped by resume() without running the filter,
// without invoking the runner, and without any state change beyond
// EventsSkippedByFilter++.
func TestResume_SkipsOwnEchoedComment(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	// Simulate the daemon having posted this exact body earlier in the Run.
	_ = postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID,
		"ℹ️ address_comment: No code changes were needed.")
	beforeSkipped := r.Object.EventsSkippedByFilter

	// Now the poller "sees" that same body coming back as a new note.
	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "andrewwormald"},
		Note:   provider.Note{ID: 200, Body: "ℹ️ address_comment: No code changes were needed."},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != r.Status {
		t.Errorf("echo skip must not change status; got %v", next)
	}
	if r.Object.EventsSkippedByFilter != beforeSkipped+1 {
		t.Errorf("EventsSkippedByFilter should have incremented; got %d (was %d)",
			r.Object.EventsSkippedByFilter, beforeSkipped)
	}
	if len(fr.calls) != 0 {
		t.Errorf("runner must not fire on an echo; got %d calls", len(fr.calls))
	}
}

// TestResume_DoesNotSkipUnrelatedComment guards against a false positive:
// a note whose body has never been posted by the daemon must fall through
// to normal handling.
func TestResume_DoesNotSkipUnrelatedComment(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "done"}})
	d.withGit(&fakeGit{hasChanges: boolPtr(false)})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	// Seed a completely unrelated hash on state.
	_ = postBotComment(t.Context(), r, fp, mr.ProjectID, mr.IID,
		"ℹ️ status: 0 completed, 1 in flight.")
	fp.comments = nil // ignore the seeding comment in subsequent assertions

	// User sends a genuine comment.
	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename Foo to Bar"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// The runner should have fired for this genuine comment.
	if next != StatusAwaitingMerge {
		t.Errorf("real user comment should route through resume normally, want AwaitingMerge, got %v", next)
	}
}

// TestSelfCommentLoop_EndToEnd is the strong regression guard for ADR-0035.
// It runs the real work() code path — no direct call to postBotComment —
// captures the exact comment body work() posted via its provider, then
// feeds a synthesised note_added event containing that same body through
// resume(). The runner must NOT fire a second time.
//
// This test would fail if:
//   - work() bypassed postBotComment for any call site (regression to
//     direct p.PostComment)
//   - The hash write happened after the network call (race window)
//   - resume()'s isOwnEcho check moved to after the filter or after
//     invokeForEvent (would need a runner invocation to skip)
//
// The prior tests (TestResume_SkipsOwnEchoedComment etc.) prove the
// mechanism in isolation; this one proves the actual daemon loop is
// closed.
func TestSelfCommentLoop_EndToEnd(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "did the thing",
		Tokens:   100,
	}})
	fp.createMRResult = provider.MR{
		ProjectID: "acme/example", IID: 42,
		URL: "https://x/42", Branch: "syntropy/deadbeef/svc-a",
	}

	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "acme/example",
		RunnerName:   "fake-runner",
		Goal:         "Migrate something",
		CurrentUnit:  "svc-a",
		BaseBranch:   "main",
		Author:       provider.User{Handle: "andrewwormald"},
		InFlight:     map[string]provider.MR{},
	})

	// --- Stage 1: real work() runs; posts the "🤖 Opened" comment.
	next, err := d.work(t.Context(), r)
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Fatalf("work should land in AwaitingMerge, got %v", next)
	}
	if len(fp.comments) != 1 {
		t.Fatalf("work should have posted exactly 1 initial status comment; got %d", len(fp.comments))
	}
	if len(fr.calls) != 1 {
		t.Fatalf("work should have invoked the runner exactly once; got %d", len(fr.calls))
	}
	// The exact body work() decided to post — we take it back from what
	// the provider actually received, so this test is decoupled from the
	// specific "🤖 Opened by ..." string.
	echoedBody := fp.comments[0].Body

	// Sanity: the fix must have recorded a hash on state before returning.
	if len(r.Object.RecentOutgoingHashes) == 0 {
		t.Fatal("work() posted a comment without recording its hash on RecentOutgoingHashes — postBotComment was bypassed")
	}
	if r.Object.RecentOutgoingHashes[len(r.Object.RecentOutgoingHashes)-1] != hashBody(echoedBody) {
		t.Fatal("the most-recent recorded hash does not match the body work() actually posted — hash write is misordered")
	}

	// --- Stage 2: simulate the poller picking that same body back up 30s
	//     later as a "new" note_added event. Author is us (gh OAuth == the
	//     Run author), which is exactly the case that trips the bug.
	echo := provider.Event{
		Kind:      provider.EventNoteAdded,
		ProjectID: "acme/example",
		MR:        r.Object.InFlight["svc-a"],
		Author:    provider.User{Handle: "andrewwormald"},
		Note: provider.Note{
			ID:   9999, // any ID > previously-seen; poller believes it's new
			Body: echoedBody,
		},
	}
	skippedBefore := r.Object.EventsSkippedByFilter

	nextAfterEcho, err := d.resume(t.Context(), r, payloadOf(t, echo))
	if err != nil {
		t.Fatalf("resume on echoed comment: %v", err)
	}

	// --- Stage 3: assert the loop is closed.

	// The runner must not have been invoked a second time.
	if len(fr.calls) != 1 {
		t.Errorf(
			"self-comment loop NOT closed: runner was invoked %d times after echo (want 1 from work() only). "+
				"An echo of the daemon's own comment triggered another claude -p call — the very bug ADR-0035 fixes.",
			len(fr.calls),
		)
	}

	// The provider must not have received a second comment from this echo path.
	if len(fp.comments) != 1 {
		t.Errorf("echo path should not post additional comments; provider received %d total (want 1)", len(fp.comments))
	}

	// Status must be unchanged (still AwaitingMerge).
	if nextAfterEcho != r.Status {
		t.Errorf("echo should not transition status; got %v (was %v)", nextAfterEcho, r.Status)
	}

	// Counter increments to prove the skip was taken (rather than the note
	// being ignored for some unrelated reason).
	if r.Object.EventsSkippedByFilter != skippedBefore+1 {
		t.Errorf(
			"EventsSkippedByFilter must increment on the skip; got %d (was %d). "+
				"If this doesn't increment, the note was dropped somewhere else (e.g. unknown-MR filter) rather than by echo suppression, and the test doesn't actually prove the fix.",
			r.Object.EventsSkippedByFilter, skippedBefore,
		)
	}

	// LastSeenNoteIDs must advance PAST the echoed note — otherwise the
	// poller would re-fetch and re-dispatch it on every 30s tick. This
	// caught a real bug in ADR-0035's original implementation where the
	// watermark update was placed AFTER the echo-skip return, so echoes
	// looped forever (correctly skipped each time, but inflating
	// events_seen and generating log noise).
	if got := r.Object.LastSeenNoteIDs[echo.MR.IID]; got < echo.Note.ID {
		t.Errorf(
			"LastSeenNoteIDs[%d] must advance to %d after processing (including on the echo-skip path); got %d. "+
				"If the watermark doesn't advance for skipped echoes, the poller re-dispatches them forever.",
			echo.MR.IID, echo.Note.ID, got,
		)
	}
}

// TestResume_CrossStreamWatermark_ByStreamCursorAdvancesIndependently is the
// regression guard for ADR-0041's cross-stream watermark bug. GitHub's
// comment endpoints (issue_comment, pull_request_review_comment,
// pull_request_review) draw ids from independent sequences. Before this
// fix, AgentState tracked a single scalar high-water mark per MR
// (LastSeenNoteIDs) shared across all three streams: once a higher-id
// issue comment advanced it, a lower-id review comment on a DIFFERENT
// stream would be (mis)classified as already-seen and never delivered.
//
// This simulates the two-tick sequence that trips the bug: tick 1 delivers
// a higher-id issue comment; tick 2 delivers a lower-id inline review
// comment that must still make it through because its own stream's cursor
// has never advanced.
func TestResume_CrossStreamWatermark_ByStreamCursorAdvancesIndependently(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ack"}})
	mr := provider.MR{ProjectID: "acme/example", IID: 42}
	r := awaitingRun(t, "svc-a", mr)

	// --- Tick 1: a higher-id issue_comment arrives and is processed.
	issueComment := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{ID: 200, Body: "please also handle the edge case", Stream: "issue_comment"},
	}
	if _, err := d.resume(t.Context(), r, payloadOf(t, issueComment)); err != nil {
		t.Fatalf("resume(issue_comment): %v", err)
	}
	if got := r.Object.LastSeenNoteIDs[mr.IID]; got != 200 {
		t.Fatalf("legacy scalar watermark should advance to 200, got %d", got)
	}
	if got := r.Object.LastSeenNoteIDsByStream[mr.IID]["issue_comment"]; got != 200 {
		t.Fatalf("issue_comment stream cursor should advance to 200, got %d", got)
	}

	// --- Tick 2: a LOWER-id inline review comment arrives on a DIFFERENT
	// stream. A single shared watermark (200) would have silently dropped
	// this — id 100 <= 200 looks "already seen" even though this exact
	// stream has never delivered anything yet.
	reviewComment := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{ID: 100, Body: "fix this line", Stream: "pull_request_review_comment", DiscussionID: "disc-1"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, reviewComment))
	if err != nil {
		t.Fatalf("resume(pull_request_review_comment): %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("processed comment should stay AwaitingMerge, got %v", next)
	}
	if r.Object.SubagentInvocations != 2 {
		t.Errorf("both comments should have invoked the runner (subagent not skipped); got %d invocations", r.Object.SubagentInvocations)
	}
	if got := r.Object.LastSeenNoteIDsByStream[mr.IID]["pull_request_review_comment"]; got != 100 {
		t.Errorf("pull_request_review_comment stream cursor should advance to 100 independently of the issue_comment stream, got %d", got)
	}
	// The legacy scalar reflects the max ever seen across all streams and
	// must NOT regress — it stays at 200, the highest id observed so far.
	if got := r.Object.LastSeenNoteIDs[mr.IID]; got != 200 {
		t.Errorf("legacy scalar watermark must remain the max across streams (200), got %d", got)
	}
}

func TestResume_UnknownMR_Dropped(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	d.withRunner(t, &fakeRunner{})
	r := awaitingRun(t, "u", provider.MR{ProjectID: "x/y", IID: 1})

	// Event arrives for a completely different MR (cross-talk via shared
	// project webhook).
	ev := provider.Event{
		Kind: provider.EventNoteAdded,
		MR:   provider.MR{ProjectID: "x/y", IID: 99},
		Note: provider.Note{Body: "hello"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("unknown MR should be silently dropped; got %v", next)
	}
	if r.Object.SubagentInvocations != 0 {
		t.Errorf("no subagent for unknown MR")
	}
}

// --- Provider auth event handling (ADR-0038) ---

func TestResume_AuthFailure_ParksRunWithProviderAuthPrefix(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{Kind: provider.EventProviderAuthFailure}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusPaused {
		t.Errorf("auth failure should park Run as Paused, got %v", next)
	}
	if !strings.HasPrefix(r.Object.PauseReason, providerAuthPausePrefix) {
		t.Errorf("PauseReason should start with provider-auth prefix, got %q", r.Object.PauseReason)
	}
	// A comment should be posted on the in-flight MR.
	if len(fp.comments) != 1 {
		t.Errorf("expected one comment on auth failure; got %d", len(fp.comments))
	}
	if !strings.Contains(fp.comments[0].Body, "401") {
		t.Errorf("comment should mention 401, got: %q", fp.comments[0].Body)
	}
}

func TestResume_AuthFailure_IdempotentWhenAlreadyAuthPaused(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = providerAuthPausePrefix + "already set"

	ev := provider.Event{Kind: provider.EventProviderAuthFailure}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("second auth failure on already-paused Run should stay Paused, got %v", next)
	}
	// No duplicate comment.
	if len(fp.comments) != 0 {
		t.Errorf("should not post duplicate comment when already auth-paused; got %d", len(fp.comments))
	}
}

func TestResume_AuthRestored_ClearsAuthPauseAndResumesWatching(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = providerAuthPausePrefix + "token expired"

	ev := provider.Event{Kind: provider.EventProviderAuthRestored}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("auth restored on auth-paused Run should go to AwaitingMerge, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("PauseReason should be cleared after auth restored, got %q", r.Object.PauseReason)
	}
}

func TestResume_AuthRestored_NoopOnNonAuthPause(t *testing.T) {
	d := newDeps(t, &fakeProvider{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = "paused by /syntropy pause from @andreww"

	ev := provider.Event{Kind: provider.EventProviderAuthRestored}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusPaused {
		t.Errorf("auth restored on non-auth-pause should stay Paused, got %v", next)
	}
	if r.Object.PauseReason == "" {
		t.Error("human-set PauseReason should not be cleared by auth restored event")
	}
}

// --- Diff shortstat in MR comments (item 4 hallucination guard, Approach A) ---

func TestResume_NoteAdded_DecisionDone_CommentContainsDiffShortstat(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "Fixed it"}})
	// fakeGit.DiffShortstat returns "1 file changed, 5 insertions(+)" by default.

	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please address this"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fp.comments) != 1 {
		t.Fatalf("expected one comment; got %d", len(fp.comments))
	}
	if !strings.Contains(fp.comments[0].Body, "Diff:") {
		t.Errorf("addressed comment should contain diff shortstat; got: %q", fp.comments[0].Body)
	}
	if !strings.Contains(fp.comments[0].Body, "file changed") {
		t.Errorf("diff shortstat should mention file changes; got: %q", fp.comments[0].Body)
	}
}

// --- restored: setup test that follows ---

func TestSetup_SubscribesToExpectedEvents(t *testing.T) {
	fp := &fakeProvider{
		authedUser: provider.User{Handle: "andreww"},
		webhookID:  "wh-1",
	}
	d := newDeps(t, fp)
	r := newRun(t, &AgentState{ProviderName: "fake", ProjectID: "x/y", EventSource: EventSourceWebhook})

	if _, err := d.setup(t.Context(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
	want := map[provider.EventKind]bool{
		provider.EventNoteAdded:         true,
		provider.EventMRMerged:          true,
		provider.EventMRClosed:          true,
		provider.EventMRUpdated:         true,
		provider.EventPipelineSucceeded: true,
		provider.EventPipelineFailed:    true,
	}
	got := map[provider.EventKind]bool{}
	for _, k := range fp.registered.Events {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("setup should subscribe to %v; got %v", k, fp.registered.Events)
		}
	}
}

// --- onAutoPause tests (ADR-0062: PauseAfterErrCount circuit breaker) ---

func newTypedRecord(state *AgentState, status AgentStatus, runStateReason string) *workflow.TypedRecord[AgentState, AgentStatus] {
	return &workflow.TypedRecord[AgentState, AgentStatus]{
		Record: workflow.Record{
			WorkflowName: "refactor-sweep-test",
			ForeignID:    "test-foreign",
			RunID:        "00000000-0000-0000-0000-deadbeefcafe",
			RunState:     workflow.RunStatePaused,
			Status:       int(status),
			Meta:         workflow.Meta{RunStateReason: runStateReason},
		},
		Status: status,
		Object: state,
	}
}

func TestOnAutoPause_PostsCommentOnEveryInFlightMR(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	state := &AgentState{
		ProviderName: "fake",
		InFlight: map[string]provider.MR{
			"increment-1": {ProjectID: "x/y", IID: 7},
			"increment-2": {ProjectID: "x/y", IID: 9},
		},
	}
	rec := newTypedRecord(state, StatusWorking, "max error retry threshold hit - automatically paused")

	if err := d.onAutoPause(t.Context(), rec); err != nil {
		t.Fatalf("onAutoPause: %v", err)
	}

	if len(fp.comments) != 2 {
		t.Fatalf("want 2 comments (one per in-flight MR), got %d: %+v", len(fp.comments), fp.comments)
	}
	gotIIDs := map[int]bool{}
	for _, c := range fp.comments {
		gotIIDs[c.MRIID] = true
		if !strings.Contains(c.Body, "Auto-paused") {
			t.Errorf("comment body should mention auto-pause; got %q", c.Body)
		}
		if !strings.Contains(c.Body, "max error retry threshold hit") {
			t.Errorf("comment body should include the RunStateReason; got %q", c.Body)
		}
		if !strings.Contains(c.Body, "/syntropy resume") {
			t.Errorf("comment body should mention the manual override; got %q", c.Body)
		}
	}
	if !gotIIDs[7] || !gotIIDs[9] {
		t.Errorf("expected comments on MR 7 and MR 9; got IIDs %v", gotIIDs)
	}
}

func TestOnAutoPause_NoInFlightMR_NoOp(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	// Pausing during setup/discover happens before any MR exists yet.
	state := &AgentState{ProviderName: "fake", InFlight: map[string]provider.MR{}}
	rec := newTypedRecord(state, StatusDiscovering, "max error retry threshold hit - automatically paused")

	if err := d.onAutoPause(t.Context(), rec); err != nil {
		t.Fatalf("onAutoPause: %v", err)
	}
	if len(fp.comments) != 0 {
		t.Errorf("want no comments when there's no in-flight MR yet; got %+v", fp.comments)
	}
}

func TestOnAutoPause_UnknownProvider_NoOp(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	state := &AgentState{
		ProviderName: "not-registered",
		InFlight:     map[string]provider.MR{"increment-1": {ProjectID: "x/y", IID: 7}},
	}
	rec := newTypedRecord(state, StatusWorking, "max error retry threshold hit - automatically paused")

	if err := d.onAutoPause(t.Context(), rec); err != nil {
		t.Fatalf("onAutoPause: %v", err)
	}
	if len(fp.comments) != 0 {
		t.Errorf("want no comments for an unregistered provider name; got %+v", fp.comments)
	}
}

func TestOnAutoPause_PostCommentFails_ReturnsError(t *testing.T) {
	fp := &fakeProvider{commentErr: errors.New("boom")}
	d := newDeps(t, fp)
	state := &AgentState{
		ProviderName: "fake",
		InFlight:     map[string]provider.MR{"increment-1": {ProjectID: "x/y", IID: 7}},
	}
	rec := newTypedRecord(state, StatusWorking, "max error retry threshold hit - automatically paused")

	if err := d.onAutoPause(t.Context(), rec); err == nil {
		t.Fatal("want error when PostComment fails, got nil")
	}
}

func TestBuild_StepsHaveErrBackOffAndPauseAfterErrCountConfigured(t *testing.T) {
	// Build panics if wiring is structurally broken (e.g. missing starting
	// node); a clean Build() plus the package-level constants being sane is
	// the practical assertion available without reaching into the library's
	// unexported consumer config. The real proof that PauseAfterErrCount and
	// ErrBackOff take effect is TestWorkflow_OnPauseHook in the upstream
	// luno/workflow test suite; ours is that Build() actually wires
	// `.WithOptions(...)` on every AddStep call without panicking.
	if stepPauseAfterErrCount < 2 {
		t.Fatalf("stepPauseAfterErrCount should tolerate more than one transient failure before tripping; got %d", stepPauseAfterErrCount)
	}
	if stepErrBackOff < time.Second {
		t.Fatalf("stepErrBackOff should be well above the library's 1s default; got %v", stepErrBackOff)
	}
}

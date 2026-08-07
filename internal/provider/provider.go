// Package provider defines the abstraction between everflow and an MR-hosting
// platform (GitLab, GitHub). Each platform supplies an implementation; the
// rest of everflow programs against this interface.
//
// See ../../DESIGN.md § "Provider abstraction" and ADR-0014, ADR-0016, ADR-0017.
package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrAuthFailure is returned (or wrapped) by provider methods when the
// platform responds with an authentication or authorisation failure (HTTP
// 401/403). The poller uses it to back off rather than hammering an expired
// token on every tick.
var ErrAuthFailure = errors.New("provider: authentication failure")

// IsAuthError reports whether err is (or wraps) ErrAuthFailure, or whether
// the error message indicates a 401/403 from the platform. The string check
// is a fallback for provider implementations that haven't yet wrapped
// ErrAuthFailure explicitly.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAuthFailure) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden")
}

// Provider is the platform abstraction. v1 ships gitlab.Provider; v2 adds
// github.Provider. All other everflow code programs against this interface.
type Provider interface {
	// Name returns the provider identifier ("gitlab", "github") used in URLs,
	// config, and logs.
	Name() string

	// AuthenticatedUser returns the user the configured credentials belong to.
	// Called once at Run trigger time to capture the Author (see ADR-0017).
	AuthenticatedUser(ctx context.Context) (User, error)

	// RegisterWebhook subscribes to project-scoped events. Returns the platform's
	// webhook ID so we can deregister later. Webhooks are project-scoped, not
	// MR-scoped — events for the whole project arrive and the daemon dispatches
	// to Runs by payload (see DESIGN.md "Provider abstraction").
	RegisterWebhook(ctx context.Context, projectID, callbackURL, secret string, events []EventKind) (webhookID string, err error)
	DeregisterWebhook(ctx context.Context, projectID, webhookID string) error

	// VerifySignature returns true iff the inbound request's HMAC matches the
	// secret we registered with. GitLab puts the bare token in X-Gitlab-Token;
	// GitHub uses X-Hub-Signature-256 with a sha256 HMAC. The provider knows.
	VerifySignature(headers http.Header, body []byte, secret string) bool

	// NormaliseEvent parses a webhook POST into our internal Event shape.
	// Returns ErrIgnore for event kinds we don't care about (e.g. push events
	// when we only subscribed to merge_requests).
	NormaliseEvent(headers http.Header, body []byte) (Event, error)

	// MR lifecycle.
	CreateMR(ctx context.Context, projectID string, mr MRDraft) (MR, error)
	PostComment(ctx context.Context, projectID string, mrIID int, body string) error
	UpdateMRTitle(ctx context.Context, projectID string, mrIID int, title string) error
	UpdateMRDescription(ctx context.Context, projectID string, mrIID int, description string) error
	CloseMR(ctx context.Context, projectID string, mrIID int) error

	// ReplyToDiscussion posts a reply within an existing comment thread,
	// instead of a new top-level MR comment, so the reviewer sees the
	// response inline against the comment it addresses. discussionID is the
	// platform-specific thread identifier surfaced in Note.DiscussionID /
	// NotePoll.DiscussionID.
	ReplyToDiscussion(ctx context.Context, projectID string, mrIID int, discussionID string, body string) error

	// ReactToNote adds an emoji reaction to a comment, used to acknowledge
	// receipt the instant everflow picks a comment up — before the
	// (potentially long) subagent invocation runs, so the author knows it
	// wasn't missed. noteID and stream identify the comment as reported on
	// Note.ID/Note.Stream or NotePoll.ID/NotePoll.Stream; emoji is a
	// platform-neutral short name (e.g. "eyes", "hourglass"). Reacting is
	// best-effort acknowledgement, not part of the durable Run state: if the
	// platform has no reaction support for this comment's stream (see
	// ADR-0050), implementations return nil rather than an error.
	ReactToNote(ctx context.Context, projectID string, mrIID int, noteID int64, stream, emoji string) error

	// GetMR fetches a single MR/PR's current identity by IID — its source
	// branch and lifecycle state. Unlike GetMRState (used by the poller's
	// tight loop and deliberately minimal), this is for one-off lookups
	// that need the branch too, e.g. `syntropy adopt` re-attaching to an
	// already-open MR whose Run record was lost, before it can check out
	// the branch or refuse a closed/merged MR.
	GetMR(ctx context.Context, projectID string, mrIID int) (MR, error)

	// Polling support (used when EventSource=poll instead of webhook).
	// GetMRState returns both the lifecycle state and mergeability from the
	// same underlying API call — HasConflict costs no extra request.
	GetMRState(ctx context.Context, projectID string, mrIID int) (MRState, error)
	ListNotesSince(ctx context.Context, projectID string, mrIID int, since NoteCursor) ([]NotePoll, error)

	// ResolveDiscussion marks a comment thread as resolved on the platform.
	// Called by invokeForEvent after a runner-driven change has been pushed
	// in response to a reviewer comment, so the reviewer sees the thread
	// closed automatically. discussionID is the platform-specific identifier
	// surfaced in Note.DiscussionID; passing an empty string is a no-op.
	ResolveDiscussion(ctx context.Context, projectID string, mrIID int, discussionID string) error

	// CI/job control.
	RetryPipelineJob(ctx context.Context, projectID string, jobID int64) error

	// User classification.
	IsBot(u User) bool
}

// EventKind names a normalised event we may subscribe to. Provider-specific
// event names map onto these.
type EventKind string

const (
	EventNoteAdded         EventKind = "note_added"
	EventPipelineSucceeded EventKind = "pipeline_succeeded"
	EventPipelineFailed    EventKind = "pipeline_failed"
	EventMRMerged          EventKind = "mr_merged"
	EventMRClosed          EventKind = "mr_closed"
	EventMRUpdated         EventKind = "mr_updated"

	// EventProviderAuthFailure is a synthetic event the poller emits when
	// it receives a 401/403 from the provider. It is never received from a
	// webhook — it signals the state machine that the token has expired so
	// it can park the Run and post a comment. See ADR-0038.
	EventProviderAuthFailure EventKind = "provider_auth_failure"

	// EventProviderAuthRestored is a synthetic event emitted by the poller
	// on the first successful API call after a prior auth failure. It clears
	// the auth-pause state and returns the Run to normal watching.
	EventProviderAuthRestored EventKind = "provider_auth_restored"

	// EventMRConflict is a synthetic event surfaced from MRState.HasConflict
	// via the existing poll interval, so the runner can resolve a merge
	// conflict without waiting for an unrelated comment/CI event to
	// incidentally surface it via SyncWithBase (see ADR-0046).
	EventMRConflict EventKind = "mr_conflict"
)

// MRState is the polled snapshot GetMRState returns: the MR's lifecycle
// state plus whether it currently has merge conflicts against its target
// branch. Both come off the same API response, so surfacing HasConflict
// alongside State costs no extra provider call.
type MRState struct {
	// State is one of "opened" | "closed" | "merged" | "locked".
	State string
	// HasConflict is true when the platform reports the MR/PR cannot be
	// merged due to a conflict with its target branch.
	HasConflict bool
}

// User is the normalised shape of a platform user. Author classification
// (ADR-0017) uses Handle to match against the Run's recorded author.
type User struct {
	ID     string
	Handle string
	Email  string
	Bot    bool
}

// MRDraft is what we hand the provider when opening a new MR.
type MRDraft struct {
	Branch       string
	TargetBranch string
	Title        string
	Description  string
	Labels       []string
	// Draft, when true, signals the platform to open the MR as Draft /
	// Work-in-Progress so it isn't accidentally reviewed or merged. GitLab
	// uses a "Draft: " title prefix; GitHub uses the draft field on create.
	Draft bool
}

// MR is a created MR's identity. Stored on AgentState alongside the unit it
// represents so inbound events can be dispatched to the right Run.
type MR struct {
	ProjectID string
	IID       int
	URL       string
	Branch    string
	// State is one of "opened" | "closed" | "merged", populated by GetMR.
	// Zero-valued ("") for MRs returned by CreateMR — callers that need the
	// state right after creation know it's freshly opened.
	State string
}

// Event is the normalised inbound event everflow's state machine consumes.
// Provider implementations parse their wire format into this shape.
type Event struct {
	Kind        EventKind
	ProjectID   string
	MR          MR
	Author      User    // the event's commenter / pusher; not the Run's author
	IsAuthor    bool    // set by everflow after normalisation, not the provider
	IsBot       bool    // mirror of Author.Bot for ergonomics
	Note        Note    // populated when Kind == EventNoteAdded
	Pipeline    Pipeline // populated for pipeline events
	Raw         []byte  // original payload for filter access; immutable
	ReceivedAt  int64   // unix nanos
}

// Note is the comment payload on a note_added event.
type Note struct {
	ID            int64
	Body          string
	DiscussionID  string // platform-specific thread identifier; pass to Provider.ResolveDiscussion
	// Stream identifies which comment endpoint this note came from
	// (provider-defined, e.g. GitHub's "issue_comment" / "review_comment" /
	// "review"; GitLab's single "note"). Used to advance the matching
	// entry in AgentState.LastSeenNoteIDsByStream — see NoteCursor.
	Stream string
}

// NoteCursor is the watermark ListNotesSince is called with. Some providers
// (GitHub) surface comments across multiple endpoints whose IDs are drawn
// from independent sequences, so a single scalar watermark can silently and
// permanently drop a comment whose ID is lower than one already seen on a
// different stream. ByStream tracks a high-water mark per provider-defined
// stream key; Legacy is the pre-migration single scalar (AgentState's old
// LastSeenNoteIDs), used as the floor for any stream not yet present in
// ByStream so already-running Runs migrate additively — see ADR-0041.
type NoteCursor struct {
	ByStream map[string]int64
	Legacy   int64
}

// NotePoll is the per-comment shape returned by ListNotesSince — used by
// the poller to synthesise note_added events. Includes the author so we
// can populate Event.Author / IsAuthor / IsBot.
type NotePoll struct {
	ID            int64
	Body          string
	Author        User
	DiscussionID  string
	// Stream identifies which comment endpoint this note came from; see
	// Note.Stream and NoteCursor.
	Stream string
}

// Pipeline is the CI payload on pipeline events.
type Pipeline struct {
	ID       int64
	Status   string // "success" | "failed" | ...
	FailedJobs []Job
}

// Job is a single CI job.
type Job struct {
	ID    int64
	Name  string
	Stage string
	Status string
	LogTail string // last ~2KB of the job log, populated for failed jobs
}

// ErrIgnore signals NormaliseEvent that this payload is not one we care about
// (e.g. a push event when we only subscribed to MR events). The caller treats
// this as a no-op, not a real error.
type ErrIgnore struct{ Reason string }

func (e ErrIgnore) Error() string { return "event ignored: " + e.Reason }

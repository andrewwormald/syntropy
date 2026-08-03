package refactorsweep

import (
	"strings"
	"testing"

	"github.com/andrewwormald/syntropy/internal/provider"
	"github.com/andrewwormald/syntropy/internal/runner"
)

// --- parser ---

func TestParseControlVerb(t *testing.T) {
	cases := []struct {
		in       string
		wantVerb string
		wantArgs string
	}{
		{"/syntropy pause", "pause", ""},
		{"  /syntropy pause  ", "pause", ""},
		{"/syntropy PAUSE", "pause", ""},
		{"/syntropy skip ran out of time", "skip", "ran out of time"},
		{"/syntropy prompt focus on auth first", "prompt", "focus on auth first"},
		{"/syntropy prompt\nuse log/slog\nnot logrus", "prompt", "use log/slog\nnot logrus"},
		{"/syntropy", "", ""},
		{"/syntropy   ", "", ""},
		{"not a command", "", ""},
		{"hello /syntropy pause", "", ""}, // must be at start
		// typo-tolerant prefix matching
		{"/syntopy pause", "pause", ""},         // dropped 'r'
		{"/suntropy pause", "pause", ""},        // 'y' -> 'u'
		{"/SYNTOPY pause", "pause", ""},         // typo + case-insensitive
		{"/syntrpoy skip out of time", "skip", "out of time"}, // transposition
		// near-words that must NOT be treated as the control prefix
		{"/synergy pause", "", ""},
		{"/syntax pause", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			v, a := parseControlVerb(tc.in)
			if v != tc.wantVerb || a != tc.wantArgs {
				t.Errorf("parseControlVerb(%q) = (%q, %q); want (%q, %q)",
					tc.in, v, a, tc.wantVerb, tc.wantArgs)
			}
		})
	}
}

// --- typo-tolerant prefix matching ---

func TestMatchControlPrefix(t *testing.T) {
	cases := []struct {
		in       string
		wantRest string
		wantOK   bool
	}{
		{"/syntropy pause", "pause", true},
		{"/syntropy", "", true},
		// realistic typos (Levenshtein distance <= 2, case-insensitive)
		{"/syntopy pause", "pause", true},   // deletion
		{"/suntropy pause", "pause", true},  // substitution
		{"/syntropyy pause", "pause", true}, // insertion
		{"/SyNtRoPy pause", "pause", true},  // mixed case
		{"/syntrpoy pause", "pause", true},  // transposition (distance 2)
		// excluded near-words — distance > 2, must not match
		{"/synergy pause", "", false},
		{"/syntax pause", "", false},
		{"/sync pause", "", false},
		// no leading slash: passthrough, never matches
		{"syntropy pause", "", false},
		{"pause", "", false},
		{"not a command", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			rest, ok := matchControlPrefix(tc.in)
			if rest != tc.wantRest || ok != tc.wantOK {
				t.Errorf("matchControlPrefix(%q) = (%q, %v); want (%q, %v)",
					tc.in, rest, ok, tc.wantRest, tc.wantOK)
			}
		})
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"syntropy", "syntropy", 0},
		{"syntropy", "syntopy", 1},
		{"syntropy", "suntropy", 1},
		{"syntropy", "synergy", 3},
		{"syntropy", "syntax", 4},
		{"", "", 0},
		{"", "abc", 3},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- helpers ---

// controlEvent builds a /syntropy comment event from the author, on the
// MR the awaitingRun helper already has in flight.
func controlEvent(body string, mr provider.MR) provider.Event {
	return provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"}, // matches awaitingRun's Author
		Note:   provider.Note{Body: body},
	}
}

// --- individual verb tests ---

// TestResume_ControlCommand_ReactsBeforeHandling asserts the generic
// /syntropy dispatch path (workflow.go's second handleControlCommand call
// site) acknowledges the triggering comment with a reaction, same as the
// subagent-invocation path does.
func TestResume_ControlCommand_ReactsBeforeHandling(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"},
		Note:   provider.Note{ID: 7, Stream: "issue_comment", Body: "/syntropy pause taking lunch"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusPaused {
		t.Errorf("want Paused, got %v", next)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("want exactly 1 reaction; got %+v", fp.reactions)
	}
	got := fp.reactions[0]
	want := reactToNoteCall{ProjectID: "x/y", MRIID: 1, NoteID: 7, Stream: "issue_comment", Emoji: "eyes"}
	if got != want {
		t.Errorf("ReactToNote call = %+v, want %+v", got, want)
	}
}

func TestCmdPause(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy pause taking lunch", mr)))
	if next != StatusPaused {
		t.Errorf("want Paused, got %v", next)
	}
	if !strings.Contains(r.Object.PauseReason, "taking lunch") {
		t.Errorf("PauseReason should include args: %q", r.Object.PauseReason)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "Paused") {
		t.Errorf("expected ack comment; got %+v", fp.comments)
	}
}

func TestCmdResume(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = "was paused for a reason"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy resume", mr)))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("PauseReason should be cleared; got %q", r.Object.PauseReason)
	}
}

// TestCmdResume_NoInFlightUnits_ReturnsDiscovering covers point 4:
// cmdResume must not claim StatusAwaitingMerge (a watched, open MR) when
// the Run paused via discoverSpec's DecisionAsk — before any unit had an
// MR, so InFlight is empty. It should route back to StatusDiscovering
// instead, which is exactly the destination the workflow graph already
// declares as valid from Paused (see workflow.go's Build) but that no
// code path previously returned.
func TestCmdResume_NoInFlightUnits_ReturnsDiscovering(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    mr.ProjectID,
		RunnerName:   "fake-runner",
		Goal:         "test goal",
		Author:       provider.User{Handle: "andreww"},
		InFlight:     map[string]provider.MR{}, // no unit started yet
	})
	r.Status = StatusPaused
	r.Object.PauseReason = askPausePrefix + "which service should I start with?"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy resume", mr)))
	if next != StatusDiscovering {
		t.Errorf("want Discovering when no unit is in flight, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("PauseReason should be cleared; got %q", r.Object.PauseReason)
	}
}

// TestCmdResume_PreMRPause_ReturnsWorking covers ADR-0097: a Run paused by
// work()'s pauseWork (a transient runner/git error before any MR existed for
// CurrentUnit) must retry work() itself, not fall back to StatusDiscovering
// — which would abandon CurrentUnit and re-plan from scratch — or claim
// StatusAwaitingMerge for an MR that was never opened. The triggering event
// carries a zero-value MR, mirroring the real controlHandler synthesising a
// /syntropy resume note with no in-flight MR to attach it to.
func TestCmdResume_PreMRPause_ReturnsWorking(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "x/y",
		RunnerName:   "fake-runner",
		Goal:         "test goal",
		Author:       provider.User{Handle: "andreww"},
		CurrentUnit:  "svc-x",
		InFlight:     map[string]provider.MR{}, // pre-MR: work() hasn't opened one yet
	})
	r.Status = StatusPaused
	r.Object.PauseReason = `transient error working on unit "svc-x": git Push: connection reset`

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy resume", provider.MR{})))
	if next != StatusWorking {
		t.Errorf("want Working for a pre-MR pause, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("PauseReason should be cleared; got %q", r.Object.PauseReason)
	}
	if r.Object.CurrentUnit != "svc-x" {
		t.Errorf("CurrentUnit should be preserved for the retry; got %q", r.Object.CurrentUnit)
	}
}

// TestCmdRetry_PreMRPause_ReturnsWorking is cmdResume's counterpart for
// /syntropy retry: with no MR yet to comment on or re-trigger, "retry" is
// only meaningful as "re-run work() for CurrentUnit" here.
func TestCmdRetry_PreMRPause_ReturnsWorking(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	r := newRun(t, &AgentState{
		ProviderName: "fake",
		ProjectID:    "x/y",
		RunnerName:   "fake-runner",
		Goal:         "test goal",
		Author:       provider.User{Handle: "andreww"},
		CurrentUnit:  "svc-x",
		InFlight:     map[string]provider.MR{},
	})
	r.Status = StatusPaused
	r.Object.PauseReason = `transient error working on unit "svc-x": git EnsureBranch: dial tcp: timeout`

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy retry", provider.MR{})))
	if next != StatusWorking {
		t.Errorf("want Working for a pre-MR pause, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("PauseReason should be cleared; got %q", r.Object.PauseReason)
	}
}

func TestCmdSkip(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 42}
	r := awaitingRun(t, "svc-a", mr)

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy skip too risky", mr)))
	if next != StatusDiscovering {
		t.Errorf("want Discovering after skip, got %v", next)
	}
	if len(fp.closes) != 1 || fp.closes[0].IID != 42 {
		t.Errorf("CloseMR should have been called for MR 42; got %+v", fp.closes)
	}
	if len(r.Object.Blacklisted) != 1 || r.Object.Blacklisted[0].UnitID != "svc-a" {
		t.Errorf("svc-a should be blacklisted; got %+v", r.Object.Blacklisted)
	}
	if !strings.Contains(r.Object.Blacklisted[0].Reason, "too risky") {
		t.Errorf("blacklist reason should include args: %q", r.Object.Blacklisted[0].Reason)
	}
}

func TestCmdSkip_UnknownMR(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	otherMR := provider.MR{ProjectID: "x/y", IID: 99}
	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy skip", otherMR)))
	if next == StatusDiscovering {
		t.Errorf("skip on untracked MR should not transition; got %v", next)
	}
	if len(fp.closes) != 0 {
		t.Errorf("untracked MR should not be closed; got %+v", fp.closes)
	}
}

func TestCmdRetry(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = "push failed"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy retry", mr)))
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge after retry, got %v", next)
	}
	if r.Object.PauseReason != "" {
		t.Errorf("retry should clear PauseReason; got %q", r.Object.PauseReason)
	}
}

func TestCmdPrompt_StoresInjection(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	body := "/syntropy prompt focus on tests first, then the lint errors"
	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent(body, mr)))
	if next != StatusAwaitingMerge {
		t.Errorf("prompt should not change state when in AwaitingMerge; got %v", next)
	}
	if !strings.Contains(r.Object.PromptInjection, "focus on tests first") {
		t.Errorf("PromptInjection not stored: %q", r.Object.PromptInjection)
	}
}

func TestCmdPrompt_NoArgs(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy prompt", mr)))
	if r.Object.PromptInjection != "" {
		t.Errorf("bare /syntropy prompt should not set injection; got %q", r.Object.PromptInjection)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "needs text") {
		t.Errorf("expected error comment; got %+v", fp.comments)
	}
}

func TestPromptInjection_ConsumedByNextRunnerCall(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionDone, Summary: "ok"}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Object.PromptInjection = "remember to handle the nil case"

	// A normal comment (not a control verb) triggers invokeForEvent. The
	// runner's Goal should now carry the injected prompt; PromptInjection
	// should be cleared after.
	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"},
		Note:   provider.Note{Body: "please rename"},
	}
	d.resume(t.Context(), r, payloadOf(t, ev))

	if len(fr.calls) != 1 {
		t.Fatalf("want 1 runner call, got %d", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].Goal, "nil case") {
		t.Errorf("Goal should carry the injected prompt: %q", fr.calls[0].Goal)
	}
	if r.Object.PromptInjection != "" {
		t.Errorf("PromptInjection should be cleared after use; got %q", r.Object.PromptInjection)
	}
}

func TestCmdStatus_PostsSummary(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Object.Goal = "Migrate to slog"
	r.Object.Completed = []CompletedUnit{{UnitID: "a"}, {UnitID: "b"}}
	r.Object.SubagentInvocations = 12

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy status", mr)))
	if len(fp.comments) != 1 {
		t.Fatalf("expected one status comment; got %+v", fp.comments)
	}
	body := fp.comments[0].Body
	for _, want := range []string{"Migrate to slog", "2 completed", "Subagent invocations: 12"} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing %q; got:\n%s", want, body)
		}
	}
}

func TestCmdStop(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 7}
	r := awaitingRun(t, "u", mr)
	r.Object.BaseRepo = "/tmp/fake"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy stop done with this", mr)))
	if next != StatusCancelled {
		t.Errorf("want Cancelled, got %v", next)
	}
	if len(fp.closes) != 1 || fp.closes[0].IID != 7 {
		t.Errorf("in-flight MRs should be closed on stop; got %+v", fp.closes)
	}
	if !strings.Contains(r.Object.LastError, "/syntropy stop") {
		t.Errorf("LastError should record cancellation: %q", r.Object.LastError)
	}
	// Verify the final comment got posted (before the close to maximise
	// visibility).
	foundStop := false
	for _, c := range fp.comments {
		if strings.Contains(c.Body, "Stopped") {
			foundStop = true
			break
		}
	}
	if !foundStop {
		t.Errorf("expected a 'Stopped' final comment; got %+v", fp.comments)
	}
}

func TestCmdHelp_BareEverflow(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy", mr)))
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "control verbs") {
		t.Errorf("bare /syntropy should post help; got %+v", fp.comments)
	}
}

func TestCmdFreeform_InvokesSubagent(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{resp: runner.Response{
		Decision: DecisionDone,
		Summary:  "refactored the auth module",
	}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	next, err := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy refactor the auth module first", mr)))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("want AwaitingMerge, got %v", next)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner should be invoked once for a freeform verb; got %d calls", len(fr.calls))
	}
	if !strings.Contains(fr.calls[0].Goal, "refactor the auth module first") {
		t.Errorf("freeform instruction not injected into Goal: %q", fr.calls[0].Goal)
	}
	if r.Object.PromptInjection != "" {
		t.Errorf("PromptInjection should be consumed (single-use); got %q", r.Object.PromptInjection)
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "refactored the auth module") {
		t.Errorf("status comment should be posted on DecisionDone; got %+v", fp.comments)
	}
}

func TestCmdFreeform_UntrackedMR_RepliesWithHelp(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	fr := d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 999} // not tracked by any in-flight unit
	r := awaitingRun(t, "u", provider.MR{ProjectID: "x/y", IID: 1})

	d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy foobar", mr)))
	if len(fr.calls) != 0 {
		t.Errorf("runner should not be invoked for an untracked MR; got %d calls", len(fr.calls))
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "isn't tracked") {
		t.Errorf("untracked MR should get a polite reply; got %+v", fp.comments)
	}
}

func TestNonAuthor_ControlComment_FallsThrough(t *testing.T) {
	// A reviewer typing /syntropy pause should NOT pause the Run — they
	// have no control privileges (ADR-0017). The /syntropy detection only
	// triggers for the Run author.
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{resp: runner.Response{Decision: DecisionNoChange, Summary: "noted"}})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "imposter"}, // NOT the author
		Note:   provider.Note{Body: "/syntropy pause"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next == StatusPaused {
		t.Errorf("non-author /syntropy should not pause; got %v", next)
	}
}

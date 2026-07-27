package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewwormald/syntropy/internal/runner"
)

func TestParseDecision_Done(t *testing.T) {
	out := `Sure, I'll do this.

[work happens]

I've updated the file.

<everflow-decision>done</everflow-decision>`
	d, summary, q, title, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done, got %v", d)
	}
	if !strings.Contains(summary, "updated the file") {
		t.Errorf("Summary should carry the body: %q", summary)
	}
	if q != "" {
		t.Errorf("Question should be empty for Done; got %q", q)
	}
	if title != "" {
		t.Errorf("Title should be empty when the model didn't include one; got %q", title)
	}
}

func TestParseDecision_DoneWithTitle(t *testing.T) {
	out := `I've migrated the logrus calls to slog.

<everflow-decision>done: feat(payments): migrate logging to slog</everflow-decision>`
	d, summary, _, title, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done, got %v", d)
	}
	if !strings.Contains(summary, "migrated the logrus calls") {
		t.Errorf("Summary should carry the body: %q", summary)
	}
	if title != "feat(payments): migrate logging to slog" {
		t.Errorf("Title not extracted: %q", title)
	}
}

func TestParseDecision_Continue(t *testing.T) {
	out := `Planning next: migrate services/payments to slog.

<everflow-decision>continue</everflow-decision>`
	d, summary, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionContinue {
		t.Errorf("Decision: want Continue, got %v", d)
	}
	if !strings.Contains(summary, "migrate services/payments") {
		t.Errorf("Summary should carry rationale: %q", summary)
	}
}

func TestParseDecision_AskWithQuestion(t *testing.T) {
	out := `I'm not sure about the deprecated middleware.

<everflow-decision>ask: Should I migrate the deprecated middleware too, or skip it?</everflow-decision>`
	d, _, q, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionAsk {
		t.Errorf("Decision: want Ask, got %v", d)
	}
	if !strings.Contains(q, "deprecated middleware too") {
		t.Errorf("Question not extracted: %q", q)
	}
}

func TestParseDecision_AskWithoutQuestion(t *testing.T) {
	// Defensive: if the model forgets the question, we default to
	// "(no question text)" rather than panicking.
	out := `<everflow-decision>ask:</everflow-decision>`
	d, _, q, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionAsk {
		t.Errorf("Decision: want Ask, got %v", d)
	}
	if q == "" {
		t.Errorf("Question should default to placeholder, not empty")
	}
}

func TestParseDecision_FailWithReason(t *testing.T) {
	out := `Tried three times.

<everflow-decision>fail: Could not resolve the API contract drift</everflow-decision>`
	d, summary, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionFail {
		t.Errorf("Decision: want Fail, got %v", d)
	}
	if !strings.Contains(summary, "API contract drift") {
		t.Errorf("Summary should include the reason: %q", summary)
	}
}

func TestParseDecision_RetryCIWithReason(t *testing.T) {
	out := `The failed job was a network timeout pulling a dependency, unrelated to the diff.

<syntropy-decision>retryci: transient network timeout in CI runner</syntropy-decision>`
	d, summary, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionRetryCI {
		t.Errorf("Decision: want RetryCI, got %v", d)
	}
	if !strings.Contains(summary, "transient network timeout") {
		t.Errorf("Summary should include the reason: %q", summary)
	}
}

func TestParseDecision_NoChange(t *testing.T) {
	out := `Looked at the codebase; this change is already applied.

<everflow-decision>nochange</everflow-decision>`
	d, _, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionNoChange {
		t.Errorf("Decision: want NoChange, got %v", d)
	}
}

func TestParseDecision_NoMarker(t *testing.T) {
	out := `I wrote some text but forgot the marker.`
	_, _, _, _, err := ParseDecision(out)
	if !errors.Is(err, ErrNoDecisionMarker) {
		t.Fatalf("want ErrNoDecisionMarker, got %v", err)
	}
}

func TestParseDecision_UnknownVerb(t *testing.T) {
	out := `<everflow-decision>bonkers</everflow-decision>`
	_, _, _, _, err := ParseDecision(out)
	if err == nil {
		t.Fatalf("want error for unknown verb")
	}
	if !strings.Contains(err.Error(), "unrecognised") {
		t.Errorf("error should mention unrecognised verb: %v", err)
	}
}

func TestParseDecision_LastMarkerWins(t *testing.T) {
	// The model sometimes echoes the protocol in its reasoning before
	// producing the real decision. We must pick the LAST occurrence.
	out := `Let me think. I could finish with <everflow-decision>continue</everflow-decision>
but actually I'm done now.

<everflow-decision>done</everflow-decision>`
	d, _, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done (last marker), got %v", d)
	}
}

func TestParseDecision_IncidentalMentionDoesNotHijack(t *testing.T) {
	// The model discusses the marker format in prose (sharing a line with
	// other text) before giving the real, standalone marker. The prose
	// mention must not be picked up as the decision.
	out := "I fixed the bug. For example, a summary like \"see the <everflow-decision>fail: bad</everflow-decision> tag\" used to hijack parsing.\n\n<everflow-decision>done</everflow-decision>"
	d, summary, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done (real marker), got %v", d)
	}
	if !strings.Contains(summary, "hijack parsing") {
		t.Errorf("Summary should carry the prose, incidental mention included: %q", summary)
	}
}

func TestParseDecision_AcceptsBothTagNames(t *testing.T) {
	// ADR-0057: the marker renamed from <everflow-decision> to
	// <syntropy-decision>, but ParseDecision must still accept the old
	// name too — an in-flight invocation prompted before a daemon
	// rebuild will still produce the old tag, and that must keep
	// parsing correctly rather than erroring.
	for _, tc := range []struct {
		name string
		out  string
	}{
		{"new tag", "All done.\n\n<syntropy-decision>done</syntropy-decision>"},
		{"legacy tag", "All done.\n\n<everflow-decision>done</everflow-decision>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, _, err := ParseDecision(tc.out)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d != runner.DecisionDone {
				t.Errorf("Decision: want Done, got %v", d)
			}
		})
	}
}

func TestParseDecision_CaseInsensitiveVerb(t *testing.T) {
	out := `<everflow-decision>DONE</everflow-decision>`
	d, _, _, _, err := ParseDecision(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("Decision: want Done, got %v", d)
	}
}

func TestBuildPrompt_AllFields(t *testing.T) {
	req := runner.Request{
		SkillCommand: "/everflow-address-comment svc-payments",
		UnitID:       "svc-payments",
		Worktree:     "/home/everflow/run-xyz/worktrees/svc-payments",
		Goal:         "Migrate logrus to log/slog. Preserve log levels.",
		CommentBody:  "please also rename the LogContext type",
		CIFailure:    "FAIL: TestSomething (panic: nil pointer)",
	}
	prompt := BuildPrompt(req)

	for _, want := range []string{
		"## Skill", "/everflow-address-comment svc-payments",
		"## Unit", "svc-payments",
		"## Worktree", "/home/everflow/run-xyz",
		"## Task", "Migrate logrus",
		"## Reviewer feedback to address", "rename the LogContext",
		"## CI failure to investigate", "TestSomething",
		"## Scope discipline", "small and narrowly scoped",
		"## How to finish", "<syntropy-decision>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
}

func TestBuildPrompt_MinimalFields(t *testing.T) {
	// Only Goal set, no UnitID — this is the planning shape. No headers
	// should be rendered for empty fields, but the decision protocol and
	// the planning flavour of scope discipline are always present.
	req := runner.Request{Goal: "just do the thing"}
	prompt := BuildPrompt(req)
	if strings.Contains(prompt, "## Skill") {
		t.Errorf("empty Skill should not produce a Skill header")
	}
	if strings.Contains(prompt, "## Reviewer feedback") {
		t.Errorf("empty CommentBody should not produce a Reviewer header")
	}
	if !strings.Contains(prompt, "just do the thing") {
		t.Errorf("Goal should be present")
	}
	if !strings.Contains(prompt, "<syntropy-decision>") {
		t.Errorf("decision protocol should always be appended")
	}
	if !strings.Contains(prompt, "## Scope discipline") {
		t.Errorf("scope discipline reminder should always be appended, even with minimal fields")
	}
	if !strings.Contains(prompt, "more, smaller increments") {
		t.Errorf("no UnitID should get the planning scope-discipline flavour")
	}
	if strings.Contains(prompt, "small and narrowly scoped") {
		t.Errorf("no UnitID should not get the unit-scoped flavour")
	}
}

func TestBuildPrompt_NonAuthorComment_AppendsTriageGuidance(t *testing.T) {
	// ADR-0072: a comment from someone other than the Run's author gets the
	// objective-defect-vs-solution-steering guidance, telling the runner to
	// route steering suggestions to Decision=Ask instead of implementing them.
	req := runner.Request{
		UnitID:            "svc-payments",
		Goal:              "do the unit",
		CommentBody:       "I'd structure this as a middleware instead",
		CommenterIsAuthor: false,
	}
	prompt := BuildPrompt(req)

	for _, want := range []string{
		"## Reviewer feedback to address",
		"not the Run's author",
		"Objective defect",
		"Solution steering",
		"Decision=Ask",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}

	// The guidance must sit with the feedback it qualifies, before the
	// scope/decision boilerplate.
	guidanceIdx := strings.Index(prompt, "not the Run's author")
	scopeIdx := strings.Index(prompt, "## Scope discipline")
	if guidanceIdx == -1 || scopeIdx == -1 || guidanceIdx >= scopeIdx {
		t.Errorf("guidance should precede scope discipline; guidanceIdx=%d scopeIdx=%d", guidanceIdx, scopeIdx)
	}
}

func TestBuildPrompt_AuthorComment_NoTriageGuidance(t *testing.T) {
	// The author steering their own Run is the normal case — no triage
	// block; their comment is implemented as before.
	req := runner.Request{
		UnitID:            "svc-payments",
		Goal:              "do the unit",
		CommentBody:       "I'd structure this as a middleware instead",
		CommenterIsAuthor: true,
	}
	prompt := BuildPrompt(req)

	if !strings.Contains(prompt, "## Reviewer feedback to address") {
		t.Errorf("author comment should still render the feedback block")
	}
	if strings.Contains(prompt, "not the Run's author") {
		t.Errorf("author comment should not get the non-author triage guidance\n--- prompt ---\n%s", prompt)
	}
}

func TestBuildPrompt_NoComment_NoTriageGuidance(t *testing.T) {
	// CommenterIsAuthor defaults to false on every non-comment invocation
	// (planning, work, fix-CI); the guidance must key off CommentBody being
	// present, not off the bool alone.
	for _, req := range []runner.Request{
		{Goal: "plan the next increment"},
		{Goal: "do the unit", UnitID: "svc-payments"},
		{Goal: "do the unit", UnitID: "svc-payments", CIFailure: "FAIL: TestSomething"},
	} {
		prompt := BuildPrompt(req)
		if strings.Contains(prompt, "not the Run's author") {
			t.Errorf("no CommentBody should mean no triage guidance; req=%+v", req)
		}
	}
}

func TestBuildPrompt_ScopeDiscipline_PlanningVsUnit(t *testing.T) {
	// req.UnitID is the discriminator between the two scope-discipline
	// flavours: empty means a planning invocation, set means a unit
	// invocation. See ADR-0045.
	planning := BuildPrompt(runner.Request{Goal: "plan the next increment"})
	if !strings.Contains(planning, "more, smaller increments") {
		t.Errorf("planning invocation (no UnitID) should get the planning flavour")
	}
	if strings.Contains(planning, "small and narrowly scoped") {
		t.Errorf("planning invocation should not get the unit flavour")
	}

	unit := BuildPrompt(runner.Request{Goal: "do the unit", UnitID: "svc-payments"})
	if !strings.Contains(unit, "small and narrowly scoped") {
		t.Errorf("unit invocation (UnitID set) should get the unit flavour")
	}
	if strings.Contains(unit, "more, smaller increments") {
		t.Errorf("unit invocation should not get the planning flavour")
	}
}

func TestBuildPrompt_ScopeDisciplinePrecedesDecisionProtocol(t *testing.T) {
	// The scope reminder must land before the decision-marker instructions,
	// so it reads as guidance on the work rather than on how to answer.
	req := runner.Request{Goal: "just do the thing"}
	prompt := BuildPrompt(req)

	scopeIdx := strings.Index(prompt, "## Scope discipline")
	decisionIdx := strings.Index(prompt, "## How to finish")
	if scopeIdx == -1 || decisionIdx == -1 {
		t.Fatalf("expected both sections present: scopeIdx=%d decisionIdx=%d", scopeIdx, decisionIdx)
	}
	if scopeIdx >= decisionIdx {
		t.Errorf("scope discipline should precede the decision protocol; scopeIdx=%d decisionIdx=%d", scopeIdx, decisionIdx)
	}
}

// TestBuildPrompt_UnitInvocation_ForbidsSelfPush guards against a real
// incident: the model ran `git push --force-with-lease` itself as an ad-hoc
// Bash command, which the target repo's own settings.json correctly denied
// (a force-push always needs human confirmation, even under
// --dangerously-skip-permissions) — but the model then never emitted a
// decision marker, leaving the turn stuck. The harness, not the model,
// owns every push.
func TestBuildPrompt_UnitInvocation_ForbidsSelfPushAndHistoryRewrite(t *testing.T) {
	unit := BuildPrompt(runner.Request{Goal: "do the unit", UnitID: "svc-payments"})
	for _, want := range []string{"git push", "force-with-lease", "harness owns every push", "commit --amend", "non-fast-forward"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit invocation prompt should mention %q; got:\n%s", want, unit)
		}
	}
}

func TestBuildPrompt_PlanningInvocation_NoSelfPushGuidance(t *testing.T) {
	// Planning is read-only by convention — it never pushes, so the
	// guidance (which lives in unitScopeDiscipline) shouldn't appear there.
	planning := BuildPrompt(runner.Request{Goal: "plan the next increment"})
	if strings.Contains(planning, "harness owns every push") {
		t.Errorf("planning invocation should not get unit-only git-push guidance")
	}
}

// TestBuildPrompt_DecisionProtocol_MandatesMarkerEvenWhenBlocked guards
// against the same incident: the model hit a denied tool call mid-turn and
// stopped with plain text asking for confirmation instead of a real
// <syntropy-decision> marker, so ParseDecision failed and the run stuck
// with an unhelpful raw-output dump instead of a clean Ask pause.
func TestBuildPrompt_DecisionProtocol_MandatesMarkerEvenWhenBlocked(t *testing.T) {
	prompt := BuildPrompt(runner.Request{Goal: "do the thing"})
	for _, want := range []string{"mandatory on every turn", "tool call you needed gets denied"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("decision protocol should mention %q; got:\n%s", want, prompt)
		}
	}
}

// --- BuildArgs tests ---

func TestBuildArgs_NoModel(t *testing.T) {
	req := runner.Request{Goal: "do the thing"}
	args := BuildArgs(req, nil)
	for _, a := range args {
		if a == "--model" {
			t.Fatalf("no --model flag expected when Model is unset; got %v", args)
		}
	}
}

func TestBuildArgs_WithModel(t *testing.T) {
	req := runner.Request{Goal: "do the thing", Model: "claude-haiku-4-5"}
	args := BuildArgs(req, nil)

	idx := -1
	for i, a := range args {
		if a == "--model" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("--model flag not found in args: %v", args)
	}
	if idx+1 >= len(args) || args[idx+1] != "claude-haiku-4-5" {
		t.Errorf("--model value: got %v", args)
	}
}

func TestBuildArgs_ExtraArgsPrepended(t *testing.T) {
	req := runner.Request{Goal: "do the thing"}
	args := BuildArgs(req, []string{"--debug"})
	if args[0] != "--debug" {
		t.Errorf("extraArgs should be prepended; got %v", args)
	}
}

func TestRunner_Name(t *testing.T) {
	if got := (&Runner{}).Name(); got != "claude" {
		t.Errorf("Name: want claude, got %q", got)
	}
	if got := NewRunner("/usr/local/bin/claude").Name(); got != "claude" {
		t.Errorf("Name: want claude regardless of binary, got %q", got)
	}
}

// --- parseJSONOutput tests ---

func TestParseJSONOutput_WithUsage(t *testing.T) {
	// Simulates `claude -p --output-format json` output when the CLI populates
	// the usage block with real token counts.
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"I updated the file.\n\n<everflow-decision>done</everflow-decision>","session_id":"sess_abc","cost_usd":0.01,"total_cost_usd":0.01,"duration_ms":3000,"num_turns":1,"usage":{"input_tokens":800,"cache_creation_input_tokens":0,"cache_read_input_tokens":200,"output_tokens":120}}`
	result, tokens, ok := parseJSONOutput(raw)
	if !ok {
		t.Fatal("parseJSONOutput returned ok=false; want ok=true")
	}
	if !strings.Contains(result, "<everflow-decision>done</everflow-decision>") {
		t.Errorf("result text should contain decision marker, got: %q", result)
	}
	// 800 + 0 + 200 + 120 = 1120
	if tokens != 1120 {
		t.Errorf("tokens: want 1120, got %d", tokens)
	}
}

func TestParseJSONOutput_NoUsage(t *testing.T) {
	// Older claude CLI build that omits the usage field should still parse
	// correctly, returning tokens=0 rather than erroring.
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"Done.\n\n<everflow-decision>done</everflow-decision>","session_id":"sess_xyz","cost_usd":0.005,"duration_ms":1500,"num_turns":1}`
	result, tokens, ok := parseJSONOutput(raw)
	if !ok {
		t.Fatal("parseJSONOutput returned ok=false; want ok=true")
	}
	if !strings.Contains(result, "Done") {
		t.Errorf("result text should include response body, got: %q", result)
	}
	if tokens != 0 {
		t.Errorf("tokens with missing usage block: want 0, got %d", tokens)
	}
}

func TestParseJSONOutput_InvalidJSON(t *testing.T) {
	// Plain-text output (e.g. from a wrapper script that doesn't use JSON mode).
	raw := "I updated the file.\n\n<everflow-decision>done</everflow-decision>"
	_, _, ok := parseJSONOutput(raw)
	if ok {
		t.Error("parseJSONOutput with plain text: want ok=false")
	}
}

func TestParseJSONOutput_WrongType(t *testing.T) {
	// JSON but not a result envelope (e.g. an error object from the platform).
	raw := `{"type":"error","message":"API rate limit exceeded"}`
	_, _, ok := parseJSONOutput(raw)
	if ok {
		t.Error("parseJSONOutput with non-result type: want ok=false")
	}
}

func TestParseJSONOutput_EmptyResult(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"result":""}`
	_, _, ok := parseJSONOutput(raw)
	if ok {
		t.Error("parseJSONOutput with empty result: want ok=false (no marker possible)")
	}
}

// TestParseDecision_FullRoundTrip checks that a real claude JSON output
// round-trips through parseJSONOutput → ParseDecision end-to-end.
func TestParseDecision_FullRoundTrip(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"I migrated the logrus calls to slog.\n\n<everflow-decision>done</everflow-decision>","session_id":"sess_rt","cost_usd":0.02,"duration_ms":4000,"num_turns":2,"usage":{"input_tokens":500,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"output_tokens":80}}`
	resultText, tokens, ok := parseJSONOutput(raw)
	if !ok {
		t.Fatal("parseJSONOutput: want ok=true")
	}
	d, summary, _, _, err := ParseDecision(resultText)
	if err != nil {
		t.Fatalf("ParseDecision: %v", err)
	}
	if d != runner.DecisionDone {
		t.Errorf("decision: want Done, got %v", d)
	}
	if !strings.Contains(summary, "migrated the logrus calls") {
		t.Errorf("summary should contain response text, got: %q", summary)
	}
	// 500 + 100 + 0 + 80 = 680
	if tokens != 680 {
		t.Errorf("tokens: want 680, got %d", tokens)
	}
}

// --- stripNestedClaudeCodeEnv tests (ADR-0064) ---

func TestStripNestedClaudeCodeEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/andreww",
		"CLAUDECODE=1",
		"CLAUDE_CODE_SESSION_ID=abc-123",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1", // not a nesting signal — must survive
		"CLAUDE_EFFORT=medium",                    // not a nesting signal — must survive
		"ANTHROPIC_API_KEY=sk-test",
	}
	got := stripNestedClaudeCodeEnv(in)

	wantKept := []string{
		"PATH=/usr/bin",
		"HOME=/home/andreww",
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1",
		"CLAUDE_EFFORT=medium",
		"ANTHROPIC_API_KEY=sk-test",
	}
	if len(got) != len(wantKept) {
		t.Fatalf("want %d entries kept, got %d: %v", len(wantKept), len(got), got)
	}
	for i, w := range wantKept {
		if got[i] != w {
			t.Errorf("entry %d: want %q, got %q", i, w, got[i])
		}
	}

	wantStripped := []string{
		"CLAUDECODE=1",
		"CLAUDE_CODE_SESSION_ID=abc-123",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_CHILD_SESSION=1",
	}
	for _, s := range wantStripped {
		for _, g := range got {
			if g == s {
				t.Errorf("want %q stripped, but it survived in %v", s, got)
			}
		}
	}
}

func TestStripNestedClaudeCodeEnv_EmptyInput(t *testing.T) {
	if got := stripNestedClaudeCodeEnv(nil); len(got) != 0 {
		t.Errorf("want empty slice for nil input, got %v", got)
	}
}

// TestRun_StripsNestedClaudeCodeEnvFromSubprocess proves Run() actually
// applies the filter to the real subprocess environment, not just that the
// helper function works in isolation. Uses a fake "claude" binary (a shell
// script) that echoes its own environment as the JSON envelope's result
// field, so the test can assert on what the subprocess actually saw.
func TestRun_StripsNestedClaudeCodeEnvFromSubprocess(t *testing.T) {
	dir := t.TempDir()
	fakeBinary := filepath.Join(dir, "fake-claude.sh")
	script := `#!/bin/sh
result=$(env | grep -c '^CLAUDE_CODE_SESSION_ID=\|^CLAUDECODE=' || true)
printf '{"type":"result","subtype":"success","is_error":false,"result":"count=%s\\n<syntropy-decision>done</syntropy-decision>"}' "$result"
`
	if err := os.WriteFile(fakeBinary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	r := &Runner{
		Binary: fakeBinary,
		Env: append(os.Environ(),
			"CLAUDECODE=1",
			"CLAUDE_CODE_SESSION_ID=some-other-session",
		),
	}
	resp, err := r.Run(context.Background(), runner.Request{Goal: "test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.Summary, "count=0") {
		t.Errorf("want the subprocess to see zero nested-session env vars, got Summary=%q", resp.Summary)
	}
}

// TestRun_NoDecisionMarker_ErrorSurfacesCleanTextNotRawJSON guards against a
// real incident: a model response with no decision marker (e.g. it hit a
// denied tool call mid-turn and just asked a plain-text question instead)
// used to produce an error whose %v was the ENTIRE raw JSON envelope — cost,
// token usage, tool-call metadata, session IDs and all — which then landed
// verbatim in a human-facing MR/PR pause comment. The error (and the
// Response.Summary it's paired with) must contain only the model's actual
// natural-language text, not the surrounding JSON.
func TestRun_NoDecisionMarker_ErrorSurfacesCleanTextNotRawJSON(t *testing.T) {
	dir := t.TempDir()
	fakeBinary := filepath.Join(dir, "fake-claude.sh")
	script := `#!/bin/sh
printf '{"type":"result","subtype":"success","is_error":false,"result":"I need your confirmation before force-pushing. OK to proceed?","total_cost_usd":0.42,"session_id":"abc-123","usage":{"input_tokens":9999}}'
`
	if err := os.WriteFile(fakeBinary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	r := &Runner{Binary: fakeBinary}
	resp, err := r.Run(context.Background(), runner.Request{Goal: "test"})
	if err == nil {
		t.Fatal("want an error when no decision marker is present")
	}
	if !strings.Contains(err.Error(), "OK to proceed?") {
		t.Errorf("error should contain the model's actual question; got: %v", err)
	}
	for _, leaked := range []string{"total_cost_usd", "session_id", "input_tokens", "0.42", "abc-123", "9999"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("error must not leak raw JSON envelope fields (found %q); got: %v", leaked, err)
		}
	}
	if resp.Summary != "I need your confirmation before force-pushing. OK to proceed?" {
		t.Errorf("Response.Summary should carry the clean text too, got %q", resp.Summary)
	}
}

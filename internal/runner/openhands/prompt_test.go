package openhands

import (
	"strings"
	"testing"

	"github.com/andrewwormald/syntropy/internal/runner"
)

// TestBuildPrompt_UnitInvocation_ForbidsSelfPushAndProviderAPI is the
// OpenHands-specific addition this package's BuildPrompt makes over
// claude.BuildPrompt: since the Agent Server's auto-approve confirmation
// policy may grant broader tool access than Claude Code's
// --dangerously-skip-permissions sandbox, the prompt must not assume the
// "never push" boundary transfers silently — it spells out git and the
// provider API explicitly.
func TestBuildPrompt_UnitInvocation_ForbidsSelfPushAndProviderAPI(t *testing.T) {
	unit := BuildPrompt(runner.Request{Goal: "do the unit", UnitID: "svc-payments"})
	for _, want := range []string{"git push", "harness owns every push", "call the GitHub/GitLab API", "never call git or the provider API directly"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit invocation prompt should mention %q; got:\n%s", want, unit)
		}
	}
}

func TestBuildPrompt_PlanningInvocation_NoSelfPushOrProviderAPIGuidance(t *testing.T) {
	planning := BuildPrompt(runner.Request{Goal: "plan the next increment"})
	for _, notWant := range []string{"harness owns every push", "call the GitHub/GitLab API"} {
		if strings.Contains(planning, notWant) {
			t.Errorf("planning invocation should not get unit-only push/provider-API guidance; got:\n%s", planning)
		}
	}
}

func TestBuildPrompt_AllFields(t *testing.T) {
	req := runner.Request{
		SkillCommand:      "/refactor-logrus-to-slog services/payments",
		UnitID:            "svc-payments",
		Worktree:          "/tmp/wt",
		Goal:              "do the thing",
		CommentBody:       "please fix this",
		CommenterIsAuthor: true,
		CIFailure:         "test failed: xyz",
		ConflictFiles:     []string{"a.go", "b.go"},
		HookFailure:       "pre-commit rejected",
		ParseFailure:      "no marker found",
		TitleConvention:   "type(scope): description",
	}
	prompt := BuildPrompt(req)
	for _, want := range []string{
		"## Skill\n\n" + req.SkillCommand,
		"## Unit\n\n" + req.UnitID,
		"## Worktree\n\n" + req.Worktree,
		"## Task\n\n" + req.Goal,
		"## Reviewer feedback to address\n\n" + req.CommentBody,
		"## CI failure to investigate",
		req.CIFailure,
		"## Merge conflict to resolve",
		"a.go",
		"## Commit rejected by pre-commit hook",
		req.HookFailure,
		"## Previous response could not be parsed",
		req.ParseFailure,
		"## MR title convention",
		req.TitleConvention,
		"## How to finish",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt should contain %q; got:\n%s", want, prompt)
		}
	}
}

func TestBuildPrompt_NonAuthorComment_AppendsTriageGuidance(t *testing.T) {
	req := runner.Request{
		Goal:              "do the thing",
		CommentBody:       "maybe use a different approach",
		CommenterIsAuthor: false,
	}
	prompt := BuildPrompt(req)
	if !strings.Contains(prompt, "Objective defect") {
		t.Errorf("non-author comment should get triage guidance; got:\n%s", prompt)
	}
}

func TestBuildPrompt_AuthorComment_NoTriageGuidance(t *testing.T) {
	req := runner.Request{
		Goal:              "do the thing",
		CommentBody:       "please fix this",
		CommenterIsAuthor: true,
	}
	prompt := BuildPrompt(req)
	if strings.Contains(prompt, "Objective defect") {
		t.Errorf("author comment should not get triage guidance; got:\n%s", prompt)
	}
}

func TestBuildPrompt_DecisionProtocol_Present(t *testing.T) {
	prompt := BuildPrompt(runner.Request{Goal: "do the thing"})
	for _, want := range []string{"mandatory on every turn", "<syntropy-decision>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("decision protocol should mention %q; got:\n%s", want, prompt)
		}
	}
}

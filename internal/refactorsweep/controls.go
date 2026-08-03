package refactorsweep

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/luno/workflow"

	"github.com/andrewwormald/syntropy/internal/provider"
)

// Control-verb dispatcher for /syntropy comments from the Run's author.
// See ADR-0017 for the privilege model and the verb set.
//
// Detected in resume() via the "/syntropy" prefix on author-comments; the
// filter never runs on these. handleControlCommand parses verb + args and
// fans out to per-verb handlers. Each handler is responsible for posting
// an acknowledgement comment so the MR thread shows what the workflow did.

// controlPrefixWord is the control-verb prefix, without its leading slash.
const controlPrefixWord = "syntropy"

// maxPrefixTypoDistance is the maximum Levenshtein distance (case-insensitive)
// a leading "/"-token may have from controlPrefixWord and still count as the
// control prefix. Chosen to catch realistic fat-finger typos (e.g. "syntopy",
// "suntropy") while staying far enough from unrelated words like "synergy"
// (distance 3) and "syntax" (distance 4) that they're never mistaken for it.
const maxPrefixTypoDistance = 2

// matchControlPrefix reports whether body starts (after trimming whitespace)
// with a "/"-token that is an exact or typo-tolerant, case-insensitive match
// for "/syntropy". On success it returns the remainder of body after that
// token, with surrounding whitespace trimmed (multi-line remainders are
// preserved as-is). ok is false if body has no leading "/"-token, or the
// token isn't within maxPrefixTypoDistance of controlPrefixWord.
func matchControlPrefix(body string) (rest string, ok bool) {
	s := strings.TrimSpace(body)
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	token := s
	if i := strings.IndexAny(s, " \t\n"); i != -1 {
		token = s[:i]
		rest = strings.TrimSpace(s[i:])
	}
	word := strings.ToLower(strings.TrimPrefix(token, "/"))
	if levenshtein(word, controlPrefixWord) > maxPrefixTypoDistance {
		return "", false
	}
	return rest, true
}

// levenshtein returns the edit distance between a and b (insertions,
// deletions, substitutions each cost 1).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

// parseControlVerb extracts (verb, args) from a comment body. The verb is
// the first whitespace-separated token after the (possibly typo'd)
// "/syntropy" prefix, lowercased. args is everything after the verb, with
// surrounding whitespace trimmed. Multi-line args are preserved.
//
// Examples:
//
//	"/syntropy pause"               → ("pause", "")
//	"/syntopy pause"                 → ("pause", "")   ← typo-tolerant
//	"/syntropy skip ran out of time" → ("skip", "ran out of time")
//	"/syntropy prompt\nuse log/slog\nnot logrus" → ("prompt", "use log/slog\nnot logrus")
//	"/syntropy"                     → ("", "")   ← bare invocation; help
//	"not a command"                 → ("", "")   ← caller is expected to gate
func parseControlVerb(body string) (verb, args string) {
	s, ok := matchControlPrefix(body)
	if !ok || s == "" {
		return "", ""
	}
	i := strings.IndexAny(s, " \t\n")
	if i == -1 {
		return strings.ToLower(s), ""
	}
	return strings.ToLower(s[:i]), strings.TrimSpace(s[i:])
}

// abandonConfirmWords are the plain-language replies StatusAwaitingAbandonConfirm
// accepts as confirmation, alongside repeating "/syntropy abandon" itself.
// Found live: the confirmation prompt asks "Are you sure?" — a yes/no
// question — but originally only accepted the exact command again, which
// reads as a different, higher-effort action than answering yes/no.
// Trimmed, trailing punctuation stripped, case-insensitive.
var abandonConfirmWords = map[string]bool{
	"yes": true, "y": true, "confirm": true, "confirmed": true,
}

// isAffirmativeConfirmation reports whether body is one of
// abandonConfirmWords, ignoring surrounding whitespace, case, and a single
// trailing '.'/'!'/'?'.
func isAffirmativeConfirmation(body string) bool {
	s := strings.ToLower(strings.TrimSpace(body))
	s = strings.TrimRight(s, ".!?")
	return abandonConfirmWords[s]
}

func (d *Deps) handleControlCommand(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event) (AgentStatus, error) {
	verb, args := parseControlVerb(ev.Note.Body)
	switch verb {
	case "pause":
		return d.cmdPause(ctx, r, ev, args)
	case "resume":
		return d.cmdResume(ctx, r, ev, args)
	case "skip":
		return d.cmdSkip(ctx, r, ev, args)
	case "retry":
		return d.cmdRetry(ctx, r, ev, args)
	case "prompt":
		return d.cmdPrompt(ctx, r, ev, args)
	case "status":
		return d.cmdStatus(ctx, r, ev, args)
	case "stop":
		return d.cmdStop(ctx, r, ev, args)
	case "abandon":
		return d.cmdAbandon(ctx, r, ev, args)
	case "":
		return d.cmdHelp(ctx, r, ev)
	default:
		return d.cmdFreeform(ctx, r, ev, verb)
	}
}

// cmdAbandon is the two-tap "are you sure?" stop. First /syntropy abandon
// transitions to StatusAwaitingAbandonConfirm with a confirmation prompt;
// a second /syntropy abandon within 12h confirms and transitions to
// StatusCancelled (closing in-flight MRs along the way). Anything else
// during the 12h window cancels the abandon — see resume()'s
// AwaitingAbandonConfirm branch and dropAbandonConfirm.
//
// Difference vs /syntropy stop: /stop is one-tap, no confirmation. Use
// /stop when you're sure; /abandon when you want a moment to reconsider.
// See ADR-0026.
func (d *Deps) cmdAbandon(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]

	if r.Status == StatusAwaitingAbandonConfirm {
		// Confirmation tap. Mirror /syntropy stop's terminal flow.
		body := fmt.Sprintf("🛑 Confirmed abandonment by @%s. Closing in-flight MRs; run cancelled.", ev.Author.Handle)
		if args != "" {
			body = fmt.Sprintf("%s\n\nReason: %s", body, args)
		}
		_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID, body)
		for unitID, mr := range r.Object.InFlight {
			_ = p.CloseMR(ctx, mr.ProjectID, mr.IID)
			d.cleanupWorktree(ctx, r, unitID)
		}
		r.Object.LastError = fmt.Sprintf("abandoned by @%s (confirmed)", ev.Author.Handle)
		return StatusCancelled, nil
	}

	// First tap — request confirmation.
	r.Object.AbandonRequestedAt = time.Now()
	body := fmt.Sprintf("⚠️ @%s requested to abandon this Run. **Are you sure?**\n\nReply `yes` (or `/syntropy abandon` again) within 12h to confirm; any other activity cancels.",
		ev.Author.Handle)
	if args != "" {
		body = fmt.Sprintf("%s\n\nReason: %s", body, args)
	}
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID, body)
	return StatusAwaitingAbandonConfirm, nil
}

// cmdPause halts forward progress. Inbound webhook events while paused
// produce no transitions except via control verbs.
func (d *Deps) cmdPause(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	reason := fmt.Sprintf("paused by /syntropy pause from @%s", ev.Author.Handle)
	if args != "" {
		reason = fmt.Sprintf("%s: %s", reason, args)
	}
	r.Object.PauseReason = reason
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
		fmt.Sprintf("🛑 Paused per @%s. Reply `/syntropy resume` to continue.", ev.Author.Handle))
	return StatusPaused, nil
}

// cmdResume clears the paused state. Which status it hands back to has to
// be honest about what's actually true:
//
//   - CurrentUnit set and InFlight empty means work() paused pre-MR (a
//     transient runner/git error — ADR-0097). There's no MR yet to watch,
//     and re-running the planner would abandon CurrentUnit for nothing;
//     StatusWorking retries work() for it directly.
//   - CurrentUnit empty and InFlight empty is discoverSpec's DecisionAsk
//     pause (askPausePrefix, ADR-0094), parked *before* any unit's MR
//     existed — markUnitMerged/markUnitBlacklisted always empty InFlight
//     before handing off to StatusDiscovering. StatusDiscovering (a valid
//     Paused-callback destination, see workflow.go's Build) is the honest
//     one — it re-invokes the planner with the author's answer now in
//     context.
//   - Otherwise there's at least one in-flight unit, so StatusAwaitingMerge
//     (claiming a webhook-watched MR exists) is correct.
func (d *Deps) cmdResume(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, _ string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	r.Object.PauseReason = ""
	var next AgentStatus
	var msg string
	switch {
	case len(r.Object.InFlight) == 0 && r.Object.CurrentUnit != "":
		next = StatusWorking
		msg = fmt.Sprintf("▶️ Resumed per @%s. Retrying work on `%s`.", ev.Author.Handle, r.Object.CurrentUnit)
	case len(r.Object.InFlight) == 0:
		next = StatusDiscovering
		msg = fmt.Sprintf("▶️ Resumed per @%s. Re-running discovery with your answer.", ev.Author.Handle)
	default:
		next = StatusAwaitingMerge
		msg = fmt.Sprintf("▶️ Resumed per @%s. Watching for events.", ev.Author.Handle)
	}
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID, msg)
	return next, nil
}

// cmdSkip blacklists the current unit and closes its MR. Refactor moves on
// to the next unit (or completes if nothing remains).
func (d *Deps) cmdSkip(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	unitID := unitForMR(r.Object.InFlight, ev.MR)
	if unitID == "" {
		_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
			"`/syntropy skip`: this MR isn't tracked by any active syntropy Run.")
		return r.Status, nil
	}

	reason := fmt.Sprintf("skipped by /syntropy skip from @%s", ev.Author.Handle)
	if args != "" {
		reason = fmt.Sprintf("%s: %s", reason, args)
	}

	_ = p.CloseMR(ctx, ev.MR.ProjectID, ev.MR.IID)
	next := d.markUnitBlacklisted(ctx, r, unitID, ev.MR, reason)
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
		fmt.Sprintf("⏭️ Skipped `%s` per @%s. MR closed; picking the next unit.", unitID, ev.Author.Handle))
	return next, nil
}

// cmdRetry clears PauseReason and unparks the Run. For a post-MR pause the
// author is responsible for re-triggering by event (re-comment, wait for CI
// to rerun, etc.) — v1 does not replay the last unsuccessful operation there.
//
// A pre-MR pause (CurrentUnit set, InFlight empty for it — work()'s
// pauseWork, ADR-0097) is different: there's no MR event to wait for, so
// this is the only "retry" available. Route straight back to StatusWorking
// to actually re-run work() for CurrentUnit, mirroring cmdResume's same
// distinction.
func (d *Deps) cmdRetry(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, _ string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	r.Object.PauseReason = ""
	if len(r.Object.InFlight) == 0 && r.Object.CurrentUnit != "" {
		_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
			fmt.Sprintf("🔄 Cleared pause per @%s. Retrying work on `%s`.", ev.Author.Handle, r.Object.CurrentUnit))
		return StatusWorking, nil
	}
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
		fmt.Sprintf("🔄 Cleared pause per @%s. Re-comment your last review feedback or wait for CI to rerun to retry the underlying operation.", ev.Author.Handle))
	return StatusAwaitingMerge, nil
}

// cmdPrompt stores an extra instruction that the next runner invocation
// (in work() or invokeForEvent) will prepend to its prompt. Single slot —
// a second /prompt overrides the first until consumed.
func (d *Deps) cmdPrompt(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	if args == "" {
		_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
			"`/syntropy prompt` needs text. Example:\n```\n/syntropy prompt focus on the auth module first\n```")
		return r.Status, nil
	}
	r.Object.PromptInjection = args
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
		fmt.Sprintf("📝 Recorded prompt from @%s. Will inject into the next subagent call:\n```\n%s\n```", ev.Author.Handle, args))
	return r.Status, nil
}

// cmdStatus posts a one-comment summary of where the Run is.
func (d *Deps) cmdStatus(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, _ string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID, buildStatusComment(r))
	return r.Status, nil
}

func buildStatusComment(r *workflow.Run[AgentState, AgentStatus]) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Run `%s` status**\n\n", shortRunID(r.RunID))
	fmt.Fprintf(&b, "- Goal: %s\n", defaultIfEmpty(r.Object.Goal, "(none)"))
	fmt.Fprintf(&b, "- State: %s\n", r.Status)
	fmt.Fprintf(&b, "- Units: %d completed, %d blacklisted, %d in-flight, %d queued\n",
		len(r.Object.Completed), len(r.Object.Blacklisted),
		len(r.Object.InFlight), len(r.Object.Queue))
	fmt.Fprintf(&b, "- Subagent invocations: %d\n", r.Object.SubagentInvocations)
	fmt.Fprintf(&b, "- Tokens used: %d", r.Object.TotalTokens)
	if r.Object.Budget.MaxTokens > 0 {
		fmt.Fprintf(&b, " / %d", r.Object.Budget.MaxTokens)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Events seen: %d (skipped by filter: %d)\n",
		r.Object.EventsSeen, r.Object.EventsSkippedByFilter)
	if r.Object.PauseReason != "" {
		fmt.Fprintf(&b, "- Pause reason: %s\n", r.Object.PauseReason)
	}
	if r.Object.LastError != "" {
		fmt.Fprintf(&b, "- Last error: %s\n", r.Object.LastError)
	}
	if r.Object.PromptInjection != "" {
		fmt.Fprintf(&b, "- Pending prompt injection: yes\n")
	}
	return b.String()
}

// cmdStop terminates the Run. Closes all in-flight MRs, cleans up worktrees,
// posts a final comment, and transitions to StatusCancelled.
func (d *Deps) cmdStop(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, args string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]

	body := fmt.Sprintf("🛑 Stopped by `/syntropy stop` from @%s. Closing in-flight MRs; run cancelled.", ev.Author.Handle)
	if args != "" {
		body = fmt.Sprintf("%s\n\nReason: %s", body, args)
	}
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID, body)

	for unitID, mr := range r.Object.InFlight {
		_ = p.CloseMR(ctx, mr.ProjectID, mr.IID)
		d.cleanupWorktree(ctx, r, unitID)
	}

	r.Object.LastError = fmt.Sprintf("cancelled by /syntropy stop from @%s", ev.Author.Handle)
	return StatusCancelled, nil
}

// cmdHelp responds to a bare "/syntropy" with the verb menu. Returns the
// current status — no transition.
func (d *Deps) cmdHelp(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID, helpMessage)
	return r.Status, nil
}

const helpMessage = "**syntropy control verbs** (author only)\n\n" +
	"- `/syntropy pause` — pause the Run\n" +
	"- `/syntropy resume` — undo pause\n" +
	"- `/syntropy skip [reason]` — blacklist this MR's unit and pick the next\n" +
	"- `/syntropy retry` — clear pause; re-trigger by next webhook event\n" +
	"- `/syntropy prompt <text>` — inject into the next subagent call\n" +
	"- `/syntropy status` — post a progress summary\n" +
	"- `/syntropy stop` — cancel the whole Run, close in-flight MRs (no confirmation)\n" +
	"- `/syntropy abandon` — request abandonment with a 12h confirmation window\n" +
	"- `/syntropy <anything else>` — treated as a freeform instruction for the subagent " +
	"(e.g. `/syntropy refactor the auth module first`); requires this MR to be tracked by an in-flight unit\n"

// cmdFreeform handles a verb that isn't one of the recognised control
// commands. Rather than bouncing "Unknown command", the whole text after
// "/syntropy " is treated as a freeform instruction for the subagent: it's
// stashed in PromptInjection (the same single-use slot /syntropy prompt
// uses) and the event is replayed through invokeForEvent immediately, as if
// it were a NoteAdded event picked up by the filter. See ADR-0042.
//
// Requires the MR to be tracked by an in-flight unit — there's no subagent
// to direct otherwise, so this falls back to a "not tracked" reply, mirroring
// cmdSkip's guard.
func (d *Deps) cmdFreeform(ctx context.Context, r *workflow.Run[AgentState, AgentStatus], ev provider.Event, verb string) (AgentStatus, error) {
	p := d.Providers[r.Object.ProviderName]
	unitID := unitForMR(r.Object.InFlight, ev.MR)
	if unitID == "" {
		_ = postBotReply(ctx, r, p, ev.MR.ProjectID, ev.MR.IID, ev.Note.DiscussionID,
			fmt.Sprintf("`/syntropy %s`: this MR isn't tracked by any active syntropy Run, so there's no subagent to direct. Reply `/syntropy` for the verb list.", verb))
		return r.Status, nil
	}

	// Recompute from the raw body (not verb+args) to preserve original
	// casing and multi-line formatting — parseControlVerb lowercases verb.
	// matchControlPrefix (not a literal "/syntropy" trim) so a typo'd
	// prefix doesn't leak into the instruction text.
	instruction, _ := matchControlPrefix(ev.Note.Body)
	r.Object.PromptInjection = instruction
	return d.invokeForEvent(ctx, r, unitID, ev)
}

package refactorsweep

import (
	"strings"
	"testing"
	"time"

	"github.com/andrewwormald/syntropy/internal/provider"
)

func TestCmdAbandon_FirstTap_RequestsConfirmation(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)

	next, err := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy abandon I'm distracted", mr)))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusAwaitingAbandonConfirm {
		t.Errorf("want StatusAwaitingAbandonConfirm, got %v", next)
	}
	if r.Object.AbandonRequestedAt.IsZero() {
		t.Errorf("AbandonRequestedAt should be set")
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "Are you sure") {
		t.Errorf("expected a confirmation prompt; got %+v", fp.comments)
	}
	if !strings.Contains(fp.comments[0].Body, "12h") {
		t.Errorf("confirmation should mention the 12h window: %q", fp.comments[0].Body)
	}
	if !strings.Contains(fp.comments[0].Body, "I'm distracted") {
		t.Errorf("reason should be echoed back: %q", fp.comments[0].Body)
	}
}

func TestCmdAbandon_SecondTap_Confirms(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 7}
	r := awaitingRun(t, "u", mr)
	// Pre-condition: already in confirmation window.
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-1 * time.Minute)

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy abandon", mr)))
	if next != StatusCancelled {
		t.Errorf("second tap should confirm → StatusCancelled, got %v", next)
	}
	if len(fp.closes) != 1 || fp.closes[0].IID != 7 {
		t.Errorf("in-flight MRs should be closed; got %+v", fp.closes)
	}
	if !strings.Contains(r.Object.LastError, "abandoned") {
		t.Errorf("LastError should record abandonment: %q", r.Object.LastError)
	}

	foundConfirmed := false
	for _, c := range fp.comments {
		if strings.Contains(c.Body, "Confirmed abandonment") {
			foundConfirmed = true
			break
		}
	}
	if !foundConfirmed {
		t.Errorf("expected a 'Confirmed abandonment' comment; got %+v", fp.comments)
	}
}

// TestCmdAbandon_SecondTap_ReactsBeforeConfirming asserts the
// AwaitingAbandonConfirm dispatch path (workflow.go's first
// handleControlCommand call site) acknowledges the confirming comment with
// a reaction, same as the generic control-command path does.
func TestCmdAbandon_SecondTap_ReactsBeforeConfirming(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 7}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-1 * time.Minute)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"},
		Note:   provider.Note{ID: 9, Stream: "issue_comment", Body: "/syntropy abandon"},
	}
	next, err := d.resume(t.Context(), r, payloadOf(t, ev))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next != StatusCancelled {
		t.Errorf("want StatusCancelled, got %v", next)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("want exactly 1 reaction; got %+v", fp.reactions)
	}
	got := fp.reactions[0]
	want := reactToNoteCall{ProjectID: "x/y", MRIID: 7, NoteID: 9, Stream: "issue_comment", Emoji: "eyes"}
	if got != want {
		t.Errorf("ReactToNote call = %+v, want %+v", got, want)
	}
}

// Regression: the confirmation prompt asks "Are you sure?" — a yes/no
// question — but originally only accepted repeating the exact
// "/syntropy abandon" command as confirmation, which reads as a
// different, higher-effort action than answering yes/no. A plain "yes"
// from the author must confirm exactly like the second "/syntropy
// abandon" tap does.
func TestCmdAbandon_PlainYesReply_Confirms(t *testing.T) {
	tests := []string{"yes", "Yes", "YES", "y", "confirm", "confirmed", "yes.", "yes!", " yes "}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			fp := &fakeProvider{}
			d := newDeps(t, fp)
			d.withRunner(t, &fakeRunner{})
			mr := provider.MR{ProjectID: "x/y", IID: 7}
			r := awaitingRun(t, "u", mr)
			r.Status = StatusAwaitingAbandonConfirm
			r.Object.AbandonRequestedAt = time.Now().Add(-1 * time.Minute)

			ev := provider.Event{
				Kind:   provider.EventNoteAdded,
				MR:     mr,
				Author: provider.User{Handle: "andreww"},
				Note:   provider.Note{Body: body},
			}
			next, err := d.resume(t.Context(), r, payloadOf(t, ev))
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if next != StatusCancelled {
				t.Errorf("plain %q reply should confirm → StatusCancelled, got %v", body, next)
			}
			if len(fp.closes) != 1 || fp.closes[0].IID != 7 {
				t.Errorf("in-flight MRs should be closed; got %+v", fp.closes)
			}
			foundConfirmed := false
			for _, c := range fp.comments {
				if strings.Contains(c.Body, "Confirmed abandonment") {
					foundConfirmed = true
				}
			}
			if !foundConfirmed {
				t.Errorf("expected a 'Confirmed abandonment' comment; got %+v", fp.comments)
			}
		})
	}
}

// A non-author's "yes" must not confirm — only the Run's author can
// confirm abandonment, same privilege rule as the /syntropy abandon path.
func TestCmdAbandon_NonAuthorPlainYes_DoesNotConfirm(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-1 * time.Minute)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "reviewer"}, // not the author
		Note:   provider.Note{Body: "yes"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("non-author 'yes' must drop back to AwaitingMerge (cancelled), got %v", next)
	}
}

// A reply that merely mentions "yes" as part of a longer sentence must
// not be mistaken for confirmation — only an exact (trimmed, punctuation-
// stripped) affirmative word confirms.
func TestCmdAbandon_YesWithinLongerSentence_DoesNotConfirm(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-1 * time.Minute)

	ev := provider.Event{
		Kind:   provider.EventNoteAdded,
		MR:     mr,
		Author: provider.User{Handle: "andreww"},
		Note:   provider.Note{Body: "yes I saw this, will look tomorrow"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("a longer sentence containing 'yes' must not confirm; want cancelled (AwaitingMerge), got %v", next)
	}
}

func TestResume_NonAbandonEventInConfirmWindow_DropsBack(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-30 * time.Minute)

	// A reviewer's normal comment during the window cancels the abandon.
	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "reviewer"}, // not the author
		Note:   provider.Note{Body: "looking at this now"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next != StatusAwaitingMerge {
		t.Errorf("non-abandon event should drop back to AwaitingMerge, got %v", next)
	}
	if !r.Object.AbandonRequestedAt.IsZero() {
		t.Errorf("AbandonRequestedAt should be cleared")
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "abandon cancelled") {
		t.Errorf("expected an 'abandon cancelled' ack; got %+v", fp.comments)
	}
}

func TestResume_NonAuthorAbandonInConfirmWindow_DoesNotConfirm(t *testing.T) {
	// If a non-author posts /syntropy abandon during the window, it must
	// NOT confirm — only the original author can. Non-author /syntropy
	// activity falls through to the dropAbandonConfirm path.
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-5 * time.Minute)

	ev := provider.Event{
		Kind: provider.EventNoteAdded, MR: mr,
		Author: provider.User{Handle: "imposter"}, // not the author
		Note:   provider.Note{Body: "/syntropy abandon"},
	}
	next, _ := d.resume(t.Context(), r, payloadOf(t, ev))
	if next == StatusCancelled {
		t.Errorf("non-author /syntropy abandon should not confirm; got %v", next)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("non-author event should drop back to AwaitingMerge, got %v", next)
	}
}

func TestResume_OtherControlVerbInConfirmWindow_DropsBack(t *testing.T) {
	// Even author /syntropy pause during the window cancels the abandon
	// (rather than honoring the pause). The semantics are restrictive on
	// purpose: only /syntropy abandon confirms. See ADR-0026.
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-5 * time.Minute)

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy pause", mr)))
	if next != StatusAwaitingMerge {
		t.Errorf("/syntropy pause during confirm window should drop back to AwaitingMerge, got %v", next)
	}
	if r.Status == StatusPaused {
		t.Errorf("Status should not flip to Paused")
	}
}

func TestOnAbandonConfirmTimeout_DropsBackAndPostsAck(t *testing.T) {
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusAwaitingAbandonConfirm
	r.Object.AbandonRequestedAt = time.Now().Add(-12 * time.Hour)

	next, err := d.onAbandonConfirmTimeout(t.Context(), r, time.Now())
	if err != nil {
		t.Fatalf("onAbandonConfirmTimeout: %v", err)
	}
	if next != StatusAwaitingMerge {
		t.Errorf("timer should drop back to AwaitingMerge, got %v", next)
	}
	if !r.Object.AbandonRequestedAt.IsZero() {
		t.Errorf("AbandonRequestedAt should be cleared after timeout")
	}
	if len(fp.comments) != 1 || !strings.Contains(fp.comments[0].Body, "expired") {
		t.Errorf("expected an 'expired' comment; got %+v", fp.comments)
	}
}

func TestCmdAbandon_FromPaused_RequestsConfirmation(t *testing.T) {
	// /syntropy abandon from a Paused Run should also enter the
	// confirmation window — the question "are you sure?" applies regardless
	// of current state.
	fp := &fakeProvider{}
	d := newDeps(t, fp)
	d.withRunner(t, &fakeRunner{})
	mr := provider.MR{ProjectID: "x/y", IID: 1}
	r := awaitingRun(t, "u", mr)
	r.Status = StatusPaused
	r.Object.PauseReason = "review feedback unresolved"

	next, _ := d.resume(t.Context(), r, payloadOf(t, controlEvent("/syntropy abandon", mr)))
	if next != StatusAwaitingAbandonConfirm {
		t.Errorf("abandon from Paused should transition to AwaitingAbandonConfirm, got %v", next)
	}
	if r.Object.AbandonRequestedAt.IsZero() {
		t.Errorf("AbandonRequestedAt should be set")
	}
}

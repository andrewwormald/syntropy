# ADR-0105: GitLab `ListNotesSince` sources from `/discussions`, not `/notes`

**Status**: Accepted
**Date**: 2026-08-05

## Context

`(*Provider).ListNotesSince` (`internal/provider/gitlab/gitlab.go`) polled
GitLab's flat `GET .../merge_requests/:iid/notes` endpoint and copied each
note's `discussion_id` field straight into `provider.NotePoll.DiscussionID`.
That field is what `ReplyToDiscussion` later uses to thread a runner's reply
under the same discussion instead of posting a new top-level comment.

GitLab's `/notes` endpoint only reliably populates `discussion_id` for
plain top-level notes. Inline diff comments (`DiffNote`s, i.e. a reviewer
commenting on a specific line in the diff) come back from `/notes` with
`discussion_id` empty. Every note — diff or plain — always has an owning
discussion once you fetch it via `GET .../merge_requests/:iid/discussions`,
where each discussion object carries its own `id` and nests its notes
underneath. `/notes` is a flattened, denormalized view of the same data
that drops this for diff notes.

The practical effect: a reviewer leaves an inline comment on a diff line,
the runner replies, and because `DiscussionID` came back empty from
`/notes`, the reply lands as an unrelated new top-level comment instead of
threading under the reviewer's inline comment.

## Decision

`ListNotesSince` now calls `GET .../merge_requests/:iid/discussions`
instead of `.../notes`. The response is `[]{id, notes: [...]}`; each note
is tagged with its parent discussion's `id` as `DiscussionID`, so both
diff notes and top-level notes get a correct, non-empty value.

Two behavioral knock-on effects from this endpoint swap:

- `/discussions` doesn't support `sort`/`order_by` query params the way
  `/notes` does, so the previous "request `sort=desc`, then reverse" trick
  no longer applies. Notes from all discussions are now collected into one
  slice and sorted ascending by `id` with `sort.Slice` before returning.
- The per-note anonymous struct dropped its own `discussion_id` field
  (unused now — the discussion's `id` is authoritative) and gained a
  parent `Notes []struct{...}` level to mirror the nested response shape.

`streamNote`, the watermark-filtering logic (`id > sinceNoteID`), and the
`system` note skip are otherwise unchanged.

## Alternatives considered

- **Keep `/notes`, backfill missing `discussion_id` some other way.**
  Rejected: GitLab doesn't expose a client-side way to recover a diff
  note's discussion id from the flat `/notes` response — the data simply
  isn't there. `/discussions` is the only endpoint that carries it.
- **Call both `/notes` and `/discussions` and merge.** Rejected: strictly
  more requests and complexity for no benefit — `/discussions` already
  contains every note `/notes` does (each discussion's `notes` array
  covers plain notes too, tagged `individual_note: true`).

## Consequences

- Replying to an inline diff review comment now threads under that
  comment's discussion instead of always falling back to a new top-level
  comment.
- One extra `sort.Slice` call per poll; negligible given `per_page=50`.
- Added `TestListNotesSince_PopulatesDiscussionIDForDiffAndTopLevelNotes`
  and `TestListNotesSince_FiltersByWatermarkAndSortsAscending` in
  `internal/provider/gitlab/gitlab_test.go` (previously no tests covered
  this method at all).

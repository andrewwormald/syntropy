# ADR-0079: Reactive 401 retry-with-forced-refresh in the provider `do()` methods

**Status**: Accepted
**Date**: 2026-07-27

## Context

ADR-0063/0065/0067 already re-resolve the provider token fresh on every
request via `tokenSource` (re-invoking `RefreshGlabToken`/`LoadGhToken`
underneath), instead of caching a token snapshot from daemon startup.
That closed the "token cached once, goes stale behind our back" gap for
the common case: a normal request that happens to land between glab's/gh's
own lazy refresh cycles.

It didn't close a narrower timing gap: `glab`/`gh` refresh their access
token lazily, only when something invokes them. If nothing has poked
them recently, the token `tokenSource` hands back for *this* request can
still be the expired one — `RefreshGlabToken`'s own poke (ADR-0065) is
best-effort and doesn't guarantee the token it then reads is valid. The
provider had no way to recover from that within the request itself: a
401 went straight to the poller's auth-backoff/pause path (ADR-0038),
parking the whole Run and asking a human to re-login, even when a bare
retry a moment later would have succeeded fine.

That poller-level pause path should be reserved for the case that
actually needs it: a genuine interactive re-login (refresh token itself
expired/revoked, not just the access token gone stale between pokes).

## Decision

Both `gitlab.Provider.do()` and `github.Provider.do()` now retry exactly
once on a 401, with a freshly re-resolved token, before returning an
error:

1. Issue the request (moved into an unexported `doOnce` on both
   providers).
2. If the response is 401 and `tokenSource` is set, close the first
   response body and call `doOnce` again. Because `tokenSource` already
   re-resolves on every call, this second call transparently re-invokes
   `RefreshGlabToken` (GitLab) or re-shells to `gh auth token` (GitHub)
   — no separate "force refresh" API needed.
3. Return whatever the retry produces — success is silent (the caller
   never sees the first 401), and a second 401 propagates as the same
   `apiError` as before, indistinguishable from today's single-attempt
   behavior to callers.

No retry when only a static `Token` is configured: a static token can't
change between attempts, so retrying would just repeat the same 401.

This makes the poller's `EventProviderAuthFailure` (ADR-0038) — and in
turn `handleProviderAuthEvent`'s pause of the whole Run — only reachable
once a 401 has already survived a forced-refresh retry inside the
provider. Practically, that means a 401 the poller sees is now good
evidence of a genuine re-login requirement, not just a stale access
token; `handleProviderAuthEvent`'s pause message and MR comment wording
are updated to say so.

## Consequences

- Adds at most one extra HTTP round trip (plus, for GitLab, one extra
  `glab api user` poke) per request that 401s — only on the unhappy
  path, never on a normal 200 request.
- `handleProviderAuthEvent`'s existing MR-comment-on-auth-failure
  behavior is unchanged in mechanism (PR #97's proposal to drop the
  comment never merged) — only its wording changes, since it now fires
  strictly after a forced-refresh retry has already failed.
- Doesn't help at all if `tokenSource` is nil (static `Token` config) —
  expected, since there's nothing to refresh in that case.

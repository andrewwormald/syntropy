# ADR-0088: goreleaser release workflow, no Homebrew tap for now

**Status**: Accepted
**Date**: 2026-07-28

## Context

Syntropy had no release automation: a version tag did not produce
downloadable binaries. Users had to `go install` or build from source.
`main.go` already exposes `version`, `gitCommit`, and `buildTime`
package-level vars (see `versionString()`) meant to be set via
`-ldflags` at build time, but nothing populated them outside the
`0.0.1-scaffold` default.

## Decision

Add `.goreleaser.yml` and `.github/workflows/release.yml` so pushing a
tag matching `v*` builds cross-platform binaries and publishes them as
a GitHub Release. goreleaser builds `linux`/`darwin`/`windows` ×
`amd64`/`arm64`, injects `-X main.version=... -X main.gitCommit=...
-X main.buildTime=...` to match the existing vars, and archives as
`.tar.gz` (`.zip` on Windows) alongside a `checksums.txt`. The workflow
runs `goreleaser/goreleaser-action` only on tag push, with
`contents: write` permission to create the release using the
default `GITHUB_TOKEN` — no extra secrets required.

No Homebrew tap is configured. That is explicitly deferred, not an
oversight.

## Alternatives considered

- **Homebrew tap (`brews:` in goreleaser)**. Rejected for now: a tap
  needs a separate `homebrew-tap` repo and a token with write access to
  it, which is more setup than this repo currently needs for its
  install base. Revisit once there's real demand for `brew install`;
  adding a `brews:` block later is additive and doesn't require
  reworking this config.
- **Hand-rolled release workflow** (matrix-build + `actions/upload-release-asset`
  instead of goreleaser). Rejected: goreleaser already solves
  cross-compilation, archiving, checksums, and changelog generation in
  one declarative config, avoiding a bespoke matrix job to maintain.
- **`_v0` archived module included in the release build**. Rejected:
  `_v0` is a separate Go module (`_v0/go.mod`, module
  `github.com/andrewwormald/everflow-v0`) kept only for CI build/test
  coverage per the existing `ci.yml` job; it has no relationship to the
  `syntropy` binary and isn't part of what a tagged release ships.

## Consequences

- Pushing a `v*` tag now builds and publishes real binaries; before this
  there was no path from tag to release artifact.
- `version`, `gitCommit`, and `buildTime` in `main.go` are populated for
  real for the first time — `syntropy version` on a released binary
  will show accurate build info instead of the scaffold default.
- README install instructions still only describe `go install` —
  documenting the new binary releases is deliberately left to a
  follow-on unit to keep this change scoped to the release pipeline
  itself.
- Adding a Homebrew tap later only requires a `brews:` block plus a
  `HOMEBREW_TAP_GITHUB_TOKEN` secret; no changes to this ADR's decisions
  are invalidated by that addition.

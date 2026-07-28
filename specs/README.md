# specs/

Spec files (`*.spec.md`) drive `everflow start --spec <path>`. Each spec is a YAML frontmatter block + a markdown body that the planner reads to decompose a goal into a chain of small MRs.

This directory holds specs that have driven Runs against `andrewwormald/syntropy` itself (dogfooding). They stay in the repo as historical artefacts and as reference examples for how to structure your own specs. The files below predate the everflow → syntropy rename (ADR-0055); their own "Notes" describe what actually happened at the time under the old name and are left as-is — only this surrounding guidance is kept current.

## Anatomy

```
---
goal: "One-sentence description; used verbatim as the runner's Goal"
provider: gitlab | github
project: owner/repo
runner: claude
base_branch: main
base_repo: /absolute/path/to/local/clone
concurrency: 1
draft_mrs: true
status: ready | draft
---

# Human-readable goal expansion

Explanation, constraints, done-when criteria. The planner reads this
markdown body on every plan call to decide the next increment.
```

## Files

| Spec | Status | Notes |
|---|---|---|
| `design-doc-refresh.spec.md` | Merged (PRs #1, #2) | Refresh two DESIGN.md sections; validated the planner's ability to re-evaluate after each merge. |
| `v0-build-check.spec.md` | Merged (PR #3) | Ended up adding a note to `AGENTS.md` instead of the Makefile the spec asked for — mid-flight scope redirect via `/everflow prompt`. Documented as a canonical example of the comment-loop workflow. |
| `early-access-hardening.spec.md` | Merged (PRs #4, #5, #6) — but PR #5 bundled 5 items in one MR, motivating ADR-0039 | Five-item readiness pass: runner token accounting, `status`/`abandon`/`resume` CLIs, hallucination guard, poll auth-expiry backoff, troubleshooting guide. (An earlier revision included CI + CodeRabbit; that item was landed manually because runner-driven MRs into `.github/workflows/*` need a GitHub OAuth token with the `workflow` scope that the daemon's `gh auth token` fallback doesn't carry by default.) |
| `adr-0039-validation.spec.md` | Not yet triggered | Deliberate three-item shopping-list spec designed to exercise ADR-0039's planner-rationale-threading fix. If the fix works, each item lands as its own MR; if it doesn't, the runner will bundle them and recreate the mega-PR failure. |

## Writing a new spec

1. Start with a tight one-sentence `goal:`. If it takes more than one sentence, decompose into multiple specs.
2. In the body, be explicit about scope — what changes, what doesn't. Include a "Done when" section with observable criteria.
3. Reference ADRs where relevant so the planner has context.
4. For safety, keep `draft_mrs: true` on any spec that runs against a shared repo.
5. Save under `specs/<short-slug>.spec.md` and trigger with `./syntropy start --spec specs/<slug>.spec.md`.

See the existing files for concrete examples.

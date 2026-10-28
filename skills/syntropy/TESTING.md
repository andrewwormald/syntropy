# Testing syntropy

## 1. Offline (safe, no network, ~20s)
    bash <skill-dir>/tests/run-tests.sh
Fake `glab`, fake `curl`, local bare git remote. Covers update check, `--apply`, `install.sh`. Expect `passed=70 failed=0`.
Needs `git`, `jq`. Run it after any script change.

## 2. Live end-to-end (GitHub)
Ask the user first. Never use a real or work repo.
1. `gh repo create <user>/syntropy-e2e --private --add-readme --clone`
2. Add a failing workflow (`on: [pull_request]`, `run: |` block, `exit 1`) to `main`.
3. `export SYN_HOME=<scratch>/home SYN_POLL=5 SYN_SCREEN=off`
4. Write a spec (`provider: github`, `project: <user>/syntropy-e2e`, `base_repo: <clone>`).
5. `syn-start.sh` → edit worktree → `syn-open.sh` → post comments with `gh pr comment`
   (`lgtm`, `/syntopy status`, a request, an inline review comment) → `syn-wait.sh`.
6. Check: noise dropped, typo verb parsed, `--resolve` resolves the thread, `ci_failed` event has a clean log,
   `gh pr merge` gives `merged`, `syn-ctl.sh <run> skip` closes a PR.
7. `syn-cleanup.sh <run>`, then delete the repo (`gh auth refresh -s delete_repo` if needed).

## 3. Live adopt (GitHub)
Same scratch repo. Push a branch, open a PR by hand, add a comment, let the failing workflow run.
`cd <clone> && syn-adopt.sh <pr-number>` → `syn-wait.sh` gives `comments` then `ci_failed`.
Fix, `syn-push.sh`. Push a human commit in between: `syn-push.sh` must merge it, not force.
`syn-ctl.sh <run> skip` must refuse. `gh pr merge` → `merged` → `syn-next.sh` ends the run.

## Not yet verified live
Adopt mode. GitLab path. Risk screen with a real `claude -p` (`SYN_SCREEN` on). Flake auto-retry loop.
Report results to the user. Do not fix scripts silently: tell the user what you changed.

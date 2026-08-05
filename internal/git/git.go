// Package git wraps the git CLI for everflow's worktree + commit + push
// operations. See ADR-0023 for why we shell out instead of using go-git.
//
// All methods on Git take the worktree directory as their first parameter.
// Auth (SSH keys, credential helpers, GIT_ASKPASS) is the host's
// responsibility — the daemon does not manage credentials.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Git is the abstraction step-body code calls into. ExecGit is the
// production impl; tests stub it.
type Git interface {
	// EnsureBranch makes sure `dir` is a git worktree on `branchName`,
	// rooted at `baseRepo`, branched off `origin/baseBranch`. Idempotent —
	// safe to call when the worktree already exists (validates and uses it).
	EnsureBranch(ctx context.Context, dir, baseRepo, baseBranch, branchName string) error

	// HardReset fetches origin/baseBranch and forces `dir` to match it,
	// discarding any local commits or working-tree changes. Used by the
	// planning worktree to refresh between iterations so the planner
	// always sees the current state of base.
	HardReset(ctx context.Context, dir, baseBranch string) error

	// DiscardUncommitted resets `dir`'s tracked files to HEAD and removes
	// untracked files, throwing away anything left uncommitted — but keeps
	// HEAD itself untouched, so any commits the branch already has ahead of
	// origin/<baseBranch> survive. Unlike HardReset (which forces `dir` to
	// match origin/<baseBranch>, discarding local commits too), this is for
	// worktrees whose own commits must be preserved and only the leftover
	// working-tree mess needs to go.
	DiscardUncommitted(ctx context.Context, dir string) error

	// HasChanges reports whether the worktree at `dir` has uncommitted
	// modifications (staged or unstaged, including untracked files).
	HasChanges(ctx context.Context, dir string) (bool, error)

	// HasWorkBeyondBase reports whether the worktree at `dir` contains any
	// work of its own relative to origin/<baseBranch>: uncommitted
	// modifications OR commits beyond the merge-base with origin/<baseBranch>.
	//
	// This is the "did the runner do anything?" check. HasChanges alone is
	// wrong for that purpose when the runner commits its own work — the
	// tree is clean afterwards, and porcelain-only detection would discard
	// the real work as "no changes". Using the merge-base (rather than
	// comparing HEAD to origin/<baseBranch> directly) means a base branch
	// that moved forward while the unit sat idle does NOT count as work.
	//
	// The four cases:
	//   - runner committed its own work (clean tree)   → true
	//   - runner left uncommitted changes (dirty tree) → true
	//   - fresh worktree, runner did nothing           → false
	//   - base moved forward, unit idle                → false
	//
	// Purely local — reads the origin/<baseBranch> tracking ref as last
	// fetched; it does not fetch. HasChanges remains the right check for
	// the Commit flow (is there anything to stage?).
	HasWorkBeyondBase(ctx context.Context, dir, baseBranch string) (bool, error)

	// Commit stages every change in the worktree and creates a commit with
	// the given message. Returns ErrNoChanges if nothing was staged — the
	// caller decides whether that's worth treating as an error.
	Commit(ctx context.Context, dir, message string) error

	// Push pushes branchName to origin, setting upstream. Auth is the host's
	// responsibility; this method does not embed credentials in URLs.
	Push(ctx context.Context, dir, branchName string) error

	// RemoveWorktree tears down the worktree at `dir` and prunes the parent
	// repo's worktree registration. Idempotent — succeeds even if `dir`
	// is already gone.
	RemoveWorktree(ctx context.Context, baseRepo, dir string) error

	// SyncWithBase fetches origin/baseBranch and merges it into dir's current
	// branch, so the branch's own commits are preserved but base has moved
	// forward to its current tip. Unlike HardReset (which discards local
	// commits — used for the planning worktree), this is for worktrees with
	// in-flight commits of their own: it refreshes the view of base without
	// throwing that work away. Called before address-comment / fix-CI runner
	// invocations so conflict resolution never judges against a stale base
	// (see ADR-0045).
	//
	// SyncWithBase requires a clean worktree: if `dir` has uncommitted
	// changes (e.g. from an interrupted invocation) it returns an error
	// without fetching or merging, so those changes are never silently
	// merged over.
	//
	// If the merge produces conflicts, that's a legitimate outcome, not an
	// error: SyncWithBase returns nil and leaves the worktree with unmerged
	// paths for the runner to resolve as part of its turn. Only failures
	// that aren't ordinary merge conflicts (fetch failure, unknown branch,
	// etc.) are returned as errors.
	SyncWithBase(ctx context.Context, dir, baseBranch string) error

	// ConflictedFiles lists the paths git currently reports as unmerged in
	// `dir` (diff --diff-filter=U), i.e. the files left conflicted by a prior
	// SyncWithBase merge. Called after SyncWithBase so the runner's conflict-
	// resolution turn knows exactly which files to look at instead of
	// re-deriving it from a full worktree scan.
	ConflictedFiles(ctx context.Context, dir string) ([]string, error)

	// DiffShortstat returns the `--shortstat` summary of commits reachable from
	// HEAD but not from origin/<baseBranch>, e.g.
	// "3 files changed, 12 insertions(+), 4 deletions(-)".
	// Returns an empty string if HEAD == origin/<baseBranch> (no commits yet).
	// Used to append the actual diff extent to MR comments so reviewers can
	// compare the runner's summary against what was really pushed (item 4 of
	// ADR-TBD hallucination guard).
	DiffShortstat(ctx context.Context, dir, baseBranch string) (string, error)

	// IsIsolatedWorktree reports whether `dir` is a real linked git worktree
	// — i.e. it has its own git-dir distinct from the repo's shared
	// (common) git-dir. This is false for the main checkout of a repo
	// (where git-dir and common-dir are the same), even though the main
	// checkout is itself a valid git directory. Intended as a deterministic,
	// non-LLM guard checked immediately before any invocation that grants
	// filesystem-write access to a directory, so such access can never
	// land on the main checkout.
	//
	// Returns an error if `dir` doesn't exist or isn't a git directory at
	// all — callers should treat that as "cannot verify isolation", not
	// silently proceed.
	IsIsolatedWorktree(ctx context.Context, dir string) (bool, error)
}

// ErrNoChanges is returned by Commit when the worktree is clean. Callers
// that want to treat clean-runner output as a real failure should check
// against this sentinel.
var ErrNoChanges = errors.New("git: no changes to commit")

// HookRejectionError is returned by Commit when staging succeeded (there
// was something to commit) but the `git commit` invocation itself failed —
// in practice this is almost always the target repo's own pre-commit or
// commit-msg hook rejecting the commit (lint, formatting, secret scanning,
// file-size caps, etc.). Output holds the hook's raw stdout+stderr so
// callers can feed it back to the runner as Request.HookFailure (ADR-0075)
// instead of pausing for a human on every rejection.
type HookRejectionError struct {
	Output string
}

func (e *HookRejectionError) Error() string {
	return fmt.Sprintf("git: commit rejected by pre-commit hook: %s", e.Output)
}

// ExecGit shells out to the `git` binary. The zero value is usable.
type ExecGit struct {
	// Author identity for commits. If empty, falls back to the daemon's
	// host git config.
	AuthorName  string
	AuthorEmail string
}

// NewExec returns an ExecGit. Both Author fields are optional; if unset,
// commits inherit the host's `user.name` / `user.email` from .gitconfig.
func NewExec(authorName, authorEmail string) *ExecGit {
	return &ExecGit{AuthorName: authorName, AuthorEmail: authorEmail}
}

// Verify ExecGit satisfies Git at compile time.
var _ Git = (*ExecGit)(nil)

func (g *ExecGit) EnsureBranch(ctx context.Context, dir, baseRepo, baseBranch, branchName string) error {
	// If `dir` is already a git directory, treat as idempotent.
	if isGitDir(dir) {
		// Verify it's the right branch; otherwise something is amiss.
		current, err := g.currentBranch(ctx, dir)
		if err != nil {
			return fmt.Errorf("EnsureBranch: read current branch in %s: %w", dir, err)
		}
		if current != branchName {
			return fmt.Errorf("EnsureBranch: worktree %s is on %q, want %q", dir, current, branchName)
		}
		return nil
	}

	// Create the parent of `dir` if it doesn't exist yet.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("EnsureBranch: mkdir parent: %w", err)
	}

	// Fetch latest from origin so we branch off current state of baseBranch.
	// baseRepo is shared across every concurrently-running unit, so its refs
	// can be lock-contended by a sibling Run's fetch; retry rides that out
	// (see ADR-0059).
	if err := withRetry(ctx, defaultFetchRetry, isLockContention, func() error {
		return g.run(ctx, baseRepo, "fetch", "origin", baseBranch)
	}); err != nil {
		return fmt.Errorf("EnsureBranch: fetch: %w", err)
	}

	// `git worktree add -b <branch> <dir> origin/<base>` creates the worktree
	// AND the branch in one step. baseRepo's refs are shared with every
	// concurrently-running unit, so this can hit the same lock contention as
	// the fetch above; retry rides it out (ADR-0059).
	//
	// A failed attempt can leave branchName created but not attached to any
	// worktree (e.g. the branch ref landed, then registering the worktree
	// itself lost a lock race) — retrying `worktree add -b` as-is would then
	// fail immediately with "already exists" instead of riding out the
	// contention. So before every attempt, including the first, clean up
	// such an orphan if one is present.
	args := []string{"worktree", "add", "-b", branchName, dir, "origin/" + baseBranch}
	if err := withRetry(ctx, defaultFetchRetry, isLockContention, func() error {
		if err := g.cleanupOrphanedBranch(ctx, baseRepo, branchName); err != nil {
			return err
		}
		return g.run(ctx, baseRepo, args...)
	}); err != nil {
		return fmt.Errorf("EnsureBranch: worktree add: %w", err)
	}
	return nil
}

// cleanupOrphanedBranch deletes branchName in baseRepo if it already exists
// as a branch but isn't checked out in any worktree. This is the state left
// by a `worktree add -b` attempt that created the branch ref but then failed
// before registering the worktree (e.g. lock contention on the worktree
// metadata) — without this, a retry of `worktree add -b` would fail
// immediately with "already exists" rather than riding out the contention.
func (g *ExecGit) cleanupOrphanedBranch(ctx context.Context, baseRepo, branchName string) error {
	if !g.branchExists(ctx, baseRepo, branchName) {
		return nil
	}
	attached, err := g.branchHasWorktree(ctx, baseRepo, branchName)
	if err != nil {
		return fmt.Errorf("cleanupOrphanedBranch: %w", err)
	}
	if attached {
		return nil
	}
	if err := g.run(ctx, baseRepo, "branch", "-D", branchName); err != nil {
		return fmt.Errorf("cleanupOrphanedBranch: delete %s: %w", branchName, err)
	}
	return nil
}

// branchExists reports whether branchName exists as a local branch in
// baseRepo.
func (g *ExecGit) branchExists(ctx context.Context, baseRepo, branchName string) bool {
	_, err := g.runOut(ctx, baseRepo, "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	return err == nil
}

// branchHasWorktree reports whether branchName is currently checked out in
// any of baseRepo's worktrees.
func (g *ExecGit) branchHasWorktree(ctx context.Context, baseRepo, branchName string) (bool, error) {
	out, err := g.runOut(ctx, baseRepo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want := "branch refs/heads/" + branchName
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true, nil
		}
	}
	return false, nil
}

func (g *ExecGit) HardReset(ctx context.Context, dir, baseBranch string) error {
	// dir is a worktree of the shared base_repo, so its remote-tracking refs
	// live in the same .git as every sibling Run's worktree; retry rides out
	// lock contention on them (see ADR-0059).
	if err := withRetry(ctx, defaultFetchRetry, isLockContention, func() error {
		return g.run(ctx, dir, "fetch", "origin", baseBranch)
	}); err != nil {
		return fmt.Errorf("HardReset: fetch: %w", err)
	}
	if err := g.run(ctx, dir, "reset", "--hard", "origin/"+baseBranch); err != nil {
		return fmt.Errorf("HardReset: reset: %w", err)
	}
	if err := g.run(ctx, dir, "clean", "-fdx"); err != nil {
		return fmt.Errorf("HardReset: clean: %w", err)
	}
	return nil
}

func (g *ExecGit) DiscardUncommitted(ctx context.Context, dir string) error {
	if err := g.run(ctx, dir, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("DiscardUncommitted: reset: %w", err)
	}
	if err := g.run(ctx, dir, "clean", "-fd"); err != nil {
		return fmt.Errorf("DiscardUncommitted: clean: %w", err)
	}
	return nil
}

func (g *ExecGit) HasChanges(ctx context.Context, dir string) (bool, error) {
	out, err := g.runOut(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("HasChanges: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

func (g *ExecGit) HasWorkBeyondBase(ctx context.Context, dir, baseBranch string) (bool, error) {
	dirty, err := g.HasChanges(ctx, dir)
	if err != nil {
		return false, fmt.Errorf("HasWorkBeyondBase: %w", err)
	}
	if dirty {
		return true, nil
	}
	mergeBase, err := g.runOut(ctx, dir, "merge-base", "origin/"+baseBranch, "HEAD")
	if err != nil {
		return false, fmt.Errorf("HasWorkBeyondBase: merge-base: %w", err)
	}
	count, err := g.runOut(ctx, dir, "rev-list", "--count", strings.TrimSpace(mergeBase)+"..HEAD")
	if err != nil {
		return false, fmt.Errorf("HasWorkBeyondBase: rev-list: %w", err)
	}
	return strings.TrimSpace(count) != "0", nil
}

func (g *ExecGit) Commit(ctx context.Context, dir, message string) error {
	dirty, err := g.HasChanges(ctx, dir)
	if err != nil {
		return err
	}
	if !dirty {
		return ErrNoChanges
	}

	// Reset the index so a previous failed commit attempt (e.g. a
	// pre-commit hook that aborted) doesn't leave stale paths staged.
	// We're about to re-stage everything selectively below.
	if err := g.run(ctx, dir, "reset"); err != nil {
		return fmt.Errorf("Commit: reset index: %w", err)
	}

	// Stage modifications to already-tracked files. `git add -u .` does
	// not touch untracked files — those go through the binary filter below.
	if err := g.run(ctx, dir, "add", "-u", "."); err != nil {
		return fmt.Errorf("Commit: add tracked: %w", err)
	}

	// Stage untracked, non-ignored files — but skip blobs that look like
	// binary build artefacts so a runner that ran `go build` doesn't get
	// its compiled output swept into the commit. Many repos enforce this
	// via pre-commit hooks that cap file size; we filter earlier so the
	// hook never has to fire.
	untracked, err := g.runOut(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("Commit: list untracked: %w", err)
	}
	var skipped []string
	for _, name := range strings.Split(strings.TrimSpace(untracked), "\n") {
		if name == "" {
			continue
		}
		if looksLikeBinary(filepath.Join(dir, name)) {
			skipped = append(skipped, name)
			continue
		}
		if err := g.run(ctx, dir, "add", "--", name); err != nil {
			return fmt.Errorf("Commit: add %s: %w", name, err)
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "syntropy git: skipped %d untracked binary file(s): %s\n",
			len(skipped), strings.Join(skipped, ", "))
	}

	// Anything actually staged? If the runner only produced binaries and
	// they were all filtered, treat this as no-op rather than running a
	// commit that errors with "nothing to commit".
	staged, err := g.runOut(ctx, dir, "diff", "--cached", "--name-only")
	if err != nil {
		return fmt.Errorf("Commit: check staged: %w", err)
	}
	if strings.TrimSpace(staged) == "" {
		return ErrNoChanges
	}

	// Run the final commit directly (rather than via run/runOut) so a
	// rejection captures stdout as well as stderr — hook output (e.g. a
	// gofmt diff or lint explanation) commonly goes to stdout, and
	// runOut's error path discards it.
	args := append(g.identityArgs(), "commit", "-m", message)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return &HookRejectionError{Output: strings.TrimSpace(stdout.String() + "\n" + stderr.String())}
	}
	return nil
}

// looksLikeBinary returns true if the first 512 bytes of `path` contain a
// NUL byte. This is the standard "is this file binary?" heuristic git
// itself uses for diff coloring etc. — text files essentially never
// contain raw NULs in their leading bytes; compiled binaries (ELF / Mach-O
// / PE) and most other binary formats do.
func looksLikeBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [512]byte
	n, _ := f.Read(buf[:])
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true
		}
	}
	return false
}

func (g *ExecGit) Push(ctx context.Context, dir, branchName string) error {
	if err := g.run(ctx, dir, "push", "-u", "origin", branchName); err != nil {
		return fmt.Errorf("Push: %w", err)
	}
	return nil
}

func (g *ExecGit) RemoveWorktree(ctx context.Context, baseRepo, dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		// Already gone; prune any leftover registration and return.
		_ = g.run(ctx, baseRepo, "worktree", "prune")
		return nil
	}
	if err := g.run(ctx, baseRepo, "worktree", "remove", "--force", dir); err != nil {
		// Fall back to manual removal if git refuses.
		_ = os.RemoveAll(dir)
		_ = g.run(ctx, baseRepo, "worktree", "prune")
	}
	return nil
}

func (g *ExecGit) SyncWithBase(ctx context.Context, dir, baseBranch string) error {
	// Refuse to merge over uncommitted changes. Git only rejects a dirty
	// worktree when the changes overlap the merge; non-overlapping ones
	// (e.g. left by an interrupted invocation) would be silently folded
	// into the merge result, so guard upfront rather than rely on git.
	dirty, err := g.HasChanges(ctx, dir)
	if err != nil {
		return fmt.Errorf("SyncWithBase: %w", err)
	}
	if dirty {
		return fmt.Errorf("SyncWithBase: worktree %s has uncommitted changes; refusing to fetch/merge", dir)
	}
	// Same shared-refs lock contention as EnsureBranch/HardReset (ADR-0059).
	if err := withRetry(ctx, defaultFetchRetry, isLockContention, func() error {
		return g.run(ctx, dir, "fetch", "origin", baseBranch)
	}); err != nil {
		return fmt.Errorf("SyncWithBase: fetch: %w", err)
	}
	// A non-fast-forward merge creates a merge commit, which needs a
	// committer identity just like Commit does.
	mergeArgs := append(g.identityArgs(), "merge", "--no-edit", "origin/"+baseBranch)
	if err := g.run(ctx, dir, mergeArgs...); err != nil {
		// Distinguish "merge left conflicts" (expected, leave for the runner)
		// from a genuine failure (bad ref, unknown branch, etc.).
		unmerged, uErr := g.runOut(ctx, dir, "diff", "--name-only", "--diff-filter=U")
		if uErr == nil && strings.TrimSpace(unmerged) != "" {
			return nil
		}
		return fmt.Errorf("SyncWithBase: merge: %w", err)
	}
	return nil
}

func (g *ExecGit) ConflictedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := g.runOut(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("ConflictedFiles: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func (g *ExecGit) DiffShortstat(ctx context.Context, dir, baseBranch string) (string, error) {
	// `git diff --shortstat A...B` shows the diff of commits reachable from B
	// but not A — i.e. everything this branch has added beyond base.
	out, err := g.runOut(ctx, dir, "diff", "--shortstat", "origin/"+baseBranch+"...HEAD")
	if err != nil {
		return "", fmt.Errorf("DiffShortstat: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (g *ExecGit) IsIsolatedWorktree(ctx context.Context, dir string) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("IsIsolatedWorktree: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("IsIsolatedWorktree: %s is not a directory", dir)
	}

	gitDir, err := g.runOut(ctx, dir, "rev-parse", "--git-dir")
	if err != nil {
		return false, fmt.Errorf("IsIsolatedWorktree: %s is not a git directory: %w", dir, err)
	}
	commonDir, err := g.runOut(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("IsIsolatedWorktree: %w", err)
	}

	// A linked worktree's git-dir (.git/worktrees/<name>) differs from the
	// repo's common git-dir (the shared .git); the main checkout's git-dir
	// IS the common dir. Resolve both to absolute, symlink-free paths before
	// comparing — git reports --git-dir relative to `dir` for the main
	// checkout but absolute for linked worktrees, so a naive string compare
	// would misclassify.
	absGitDir, err := resolveGitPath(dir, strings.TrimSpace(gitDir))
	if err != nil {
		return false, fmt.Errorf("IsIsolatedWorktree: resolve git-dir: %w", err)
	}
	absCommonDir, err := resolveGitPath(dir, strings.TrimSpace(commonDir))
	if err != nil {
		return false, fmt.Errorf("IsIsolatedWorktree: resolve git-common-dir: %w", err)
	}
	return absGitDir != absCommonDir, nil
}

// resolveGitPath resolves a path returned by `git rev-parse --git-dir` /
// `--git-common-dir` (which may be relative to `dir` or already absolute)
// to an absolute, symlink-free path suitable for equality comparison.
func resolveGitPath(dir, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// --- retry-with-backoff (ADR-0059) ---

// retryConfig controls withRetry. sleep is a test seam — production code
// always uses time.Sleep; tests substitute a no-op or call-recording func
// so retry tests don't actually wait out the backoff.
type retryConfig struct {
	attempts  int
	baseDelay time.Duration
	sleep     func(time.Duration)
}

// defaultFetchRetry backs off 200ms, 400ms, 800ms, 1.6s across 5 attempts —
// long enough to ride out another Run's fetch holding a ref lock on the
// shared base_repo, short enough not to noticeably delay a unit that hits
// contention only once.
var defaultFetchRetry = retryConfig{
	attempts:  5,
	baseDelay: 200 * time.Millisecond,
	sleep:     time.Sleep,
}

// withRetry calls fn until it succeeds, cfg.attempts is exhausted, ctx is
// done, or fn's error isn't one isRetryable recognizes — whichever comes
// first. Delay doubles after each retryable failure, starting at
// cfg.baseDelay. The final attempt's error is returned as-is.
func withRetry(ctx context.Context, cfg retryConfig, isRetryable func(error) bool, fn func() error) error {
	delay := cfg.baseDelay
	var err error
	for attempt := 1; attempt <= cfg.attempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		if attempt == cfg.attempts || !isRetryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		default:
		}
		cfg.sleep(delay)
		delay *= 2
	}
	return err
}

// isLockContention reports whether err looks like a fetch failed because a
// sibling process (another Run fetching the same shared base_repo) held a
// ref/packed-refs lock at the same moment, rather than a real failure
// (bad remote, network down, auth). Matched against the substrings git
// itself prints for a busy lock file.
func isLockContention(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	markers := []string{
		"unable to create",    // "unable to create '.../shallow.lock': File exists"
		"cannot lock ref",     // "cannot lock ref '...': unable to resolve reference"
		"could not lock",      // "could not lock config file"
		"another git process", // "another git process seems to be running in this repository"
	}
	for _, m := range markers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// --- internal helpers ---

// identityArgs returns `-c user.name=… -c user.email=…` flags for git
// commands that create commits, so they work on hosts with no global
// git identity configured (e.g. CI runners). Empty when no author is set,
// in which case commits inherit the host's .gitconfig.
func (g *ExecGit) identityArgs() []string {
	if g.AuthorName == "" || g.AuthorEmail == "" {
		return nil
	}
	return []string{
		"-c", "user.name=" + g.AuthorName,
		"-c", "user.email=" + g.AuthorEmail,
	}
}

func (g *ExecGit) currentBranch(ctx context.Context, dir string) (string, error) {
	out, err := g.runOut(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (g *ExecGit) run(ctx context.Context, dir string, args ...string) error {
	_, err := g.runOut(ctx, dir, args...)
	return err
}

func (g *ExecGit) runOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never prompt interactively — fail fast on missing auth
	)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w; stderr: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// envProbe is a test seam that returns the env slice the runner would pass.
// Production code uses runOut directly; this exists only so the test in
// git_test.go can assert GIT_TERMINAL_PROMPT=0 is set.
func (g *ExecGit) envProbe() string {
	return strings.Join(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), "\n")
}

func isGitDir(dir string) bool {
	// Both .git directories (top-level repo) and .git files (worktree
	// metadata files) count.
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || !info.IsDir())
}

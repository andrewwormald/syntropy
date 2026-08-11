package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExecGit_FullLifecycle exercises EnsureBranch → modify file → HasChanges
// → Commit → (Push skipped — needs a remote) → RemoveWorktree against a
// real on-disk git repo set up in t.TempDir().
//
// Skips if `git` isn't on PATH (CI environments without git).
func TestExecGit_FullLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	// Initialise a real repo with one commit on main, and an "origin" remote
	// that's just a bare clone of itself — so `fetch origin main` works.
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("syntropy-test", "syntropy@test.invalid")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/svc-a"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	// Worktree should exist as a real git checkout.
	if _, err := os.Stat(filepath.Join(worktreeDir, ".git")); err != nil {
		t.Fatalf("expected .git in worktree: %v", err)
	}

	// HasChanges: should be clean.
	dirty, err := g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges (clean): %v", err)
	}
	if dirty {
		t.Errorf("HasChanges: want false on a fresh worktree, got true")
	}

	// Commit on a clean tree should return ErrNoChanges.
	if err := g.Commit(ctx, worktreeDir, "noop"); !errors.Is(err, ErrNoChanges) {
		t.Errorf("Commit on clean tree: want ErrNoChanges, got %v", err)
	}

	// Modify a file. HasChanges should flip; Commit should succeed.
	writeFile(t, worktreeDir, "NEW.md", "hello from syntropy\n")
	dirty, err = g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges (dirty): %v", err)
	}
	if !dirty {
		t.Errorf("HasChanges: want true after writing a file")
	}
	if err := g.Commit(ctx, worktreeDir, "add NEW.md"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// After commit, working tree should be clean again.
	dirty, _ = g.HasChanges(ctx, worktreeDir)
	if dirty {
		t.Errorf("HasChanges: want false after commit")
	}

	// EnsureBranch on the existing dir should be a no-op.
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/svc-a"); err != nil {
		t.Errorf("EnsureBranch should be idempotent: %v", err)
	}

	// EnsureBranch on a dir that's a worktree for a DIFFERENT branch must error.
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "wrong/branch"); err == nil {
		t.Errorf("EnsureBranch should reject worktree on wrong branch")
	}

	// RemoveWorktree should clean up.
	if err := g.RemoveWorktree(ctx, baseRepo, worktreeDir); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone after RemoveWorktree, got err=%v", err)
	}

	// RemoveWorktree is idempotent — second call succeeds.
	if err := g.RemoveWorktree(ctx, baseRepo, worktreeDir); err != nil {
		t.Errorf("RemoveWorktree should be idempotent: %v", err)
	}
}

// TestExecGit_CheckoutExistingBranch_ChecksOutBranchAlreadyOnOrigin exercises
// the `syntropy adopt` scenario: a branch already exists on origin (pushed by
// some prior, now-untracked process), and CheckoutExistingBranch should check
// it out into a fresh worktree without creating a new branch or losing the
// commit already on it.
func TestExecGit_CheckoutExistingBranch_ChecksOutBranchAlreadyOnOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	// Simulate a branch that already exists on origin, with a commit of its
	// own, but with no local Run tracking it — the "lost Run record" case.
	runMust(t, baseRepo, "checkout", "-b", "adopt/existing-mr")
	writeFile(t, baseRepo, "EXISTING.md", "already in flight\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "in-flight work")
	runMust(t, baseRepo, "push", "origin", "adopt/existing-mr")
	runMust(t, baseRepo, "checkout", "main")

	g := NewExec("syntropy-test", "syntropy@test.invalid")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.CheckoutExistingBranch(ctx, worktreeDir, baseRepo, "adopt/existing-mr"); err != nil {
		t.Fatalf("CheckoutExistingBranch: %v", err)
	}

	current, err := g.currentBranch(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("currentBranch: %v", err)
	}
	if current != "adopt/existing-mr" {
		t.Errorf("current branch = %q, want %q", current, "adopt/existing-mr")
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "EXISTING.md")); err != nil {
		t.Errorf("expected EXISTING.md (the branch's own commit) to be present: %v", err)
	}

	// Idempotent — calling again on the same worktree is a no-op.
	if err := g.CheckoutExistingBranch(ctx, worktreeDir, baseRepo, "adopt/existing-mr"); err != nil {
		t.Errorf("CheckoutExistingBranch should be idempotent: %v", err)
	}

	// Calling on a worktree that's on a different branch must error.
	if err := g.CheckoutExistingBranch(ctx, worktreeDir, baseRepo, "main"); err == nil {
		t.Errorf("CheckoutExistingBranch should reject worktree on wrong branch")
	}
}

// TestExecGit_CheckoutExistingBranch_PrunesStaleWorktreeRegistration
// reproduces the adopt incident: a prior Run died without ever calling
// RemoveWorktree, leaving baseRepo's worktree registration pointing at a
// directory that's since been deleted out from under git. Checking out the
// same branch into a new directory must still succeed rather than failing
// with "already used by worktree at ...".
func TestExecGit_CheckoutExistingBranch_PrunesStaleWorktreeRegistration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	runMust(t, baseRepo, "checkout", "-b", "adopt/existing-mr")
	writeFile(t, baseRepo, "EXISTING.md", "already in flight\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "in-flight work")
	runMust(t, baseRepo, "push", "origin", "adopt/existing-mr")
	runMust(t, baseRepo, "checkout", "main")

	g := NewExec("syntropy-test", "syntropy@test.invalid")
	ctx := t.Context()

	// Register a worktree for the branch, then delete its directory without
	// telling git — the state a killed prior Run leaves behind.
	deadDir := filepath.Join(t.TempDir(), "dead-run-worktree")
	runMust(t, baseRepo, "worktree", "add", deadDir, "adopt/existing-mr")
	if err := os.RemoveAll(deadDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.CheckoutExistingBranch(ctx, worktreeDir, baseRepo, "adopt/existing-mr"); err != nil {
		t.Fatalf("CheckoutExistingBranch: %v", err)
	}

	current, err := g.currentBranch(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("currentBranch: %v", err)
	}
	if current != "adopt/existing-mr" {
		t.Errorf("current branch = %q, want %q", current, "adopt/existing-mr")
	}
}

// TestExecGit_CheckoutExistingBranch_UnknownBranchErrors ensures a branch
// that doesn't exist on origin surfaces as an error rather than silently
// creating a new branch (which is what EnsureBranch's `-b` would do).
func TestExecGit_CheckoutExistingBranch_UnknownBranchErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("syntropy-test", "syntropy@test.invalid")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.CheckoutExistingBranch(ctx, worktreeDir, baseRepo, "does/not-exist"); err == nil {
		t.Errorf("CheckoutExistingBranch should error on a branch that doesn't exist on origin")
	}
}

func TestExecGit_HardReset_DiscardsLocalChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "v1")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/plan/abc"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Plant a local modification and an untracked file.
	writeFile(t, worktreeDir, "README.md", "tampered\n")
	writeFile(t, worktreeDir, "untracked.txt", "should be gone\n")

	if err := g.HardReset(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("HardReset: %v", err)
	}

	// README.md should be back to the original; untracked.txt should be gone.
	readme, err := os.ReadFile(filepath.Join(worktreeDir, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(readme) != "v1\n" {
		t.Errorf("README.md not restored: %q", readme)
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked.txt should have been removed; err=%v", err)
	}
}

// TestExecGit_DiscardUncommitted_KeepsCommitsButDropsWorkingTreeMess proves
// DiscardUncommitted resets tracked-file edits and removes untracked files,
// while — unlike HardReset — preserving commits the branch has made beyond
// origin/<baseBranch>.
func TestExecGit_DiscardUncommitted_KeepsCommitsButDropsWorkingTreeMess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "v1")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/plan/abc"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Give the branch a commit of its own, ahead of origin/main.
	writeFile(t, worktreeDir, "OWN.md", "own work\n")
	if err := g.Commit(ctx, worktreeDir, "add OWN.md"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	headBefore, err := g.runOut(ctx, worktreeDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	// Plant a local modification and an untracked file on top of that commit.
	writeFile(t, worktreeDir, "README.md", "tampered\n")
	writeFile(t, worktreeDir, "untracked.txt", "should be gone\n")

	if err := g.DiscardUncommitted(ctx, worktreeDir); err != nil {
		t.Fatalf("DiscardUncommitted: %v", err)
	}

	// README.md should be back to HEAD's version; untracked.txt should be gone.
	readme, err := os.ReadFile(filepath.Join(worktreeDir, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(readme) != "v1\n" {
		t.Errorf("README.md not restored: %q", readme)
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked.txt should have been removed; err=%v", err)
	}
	// OWN.md, and the commit that added it, must survive — DiscardUncommitted
	// only throws away uncommitted mess, not the branch's own commits.
	if _, err := os.Stat(filepath.Join(worktreeDir, "OWN.md")); err != nil {
		t.Errorf("OWN.md should have survived: %v", err)
	}
	headAfter, err := g.runOut(ctx, worktreeDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if headAfter != headBefore {
		t.Errorf("HEAD moved: want unchanged at %q, got %q", headBefore, headAfter)
	}
}

// TestExecGit_SyncWithBase_FastForwardsWithoutLosingLocalCommits covers the
// common case: base has moved on, the feature branch has its own commit,
// and the two don't conflict. SyncWithBase should bring base's commit in
// while keeping the feature branch's own commit intact.
func TestExecGit_SyncWithBase_FastForwardsWithoutLosingLocalCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	writeFile(t, baseRepo, "other.md", "unrelated\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/sync"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Feature branch gets its own commit, touching a file base never touches.
	writeFile(t, worktreeDir, "feature.md", "my change\n")
	if err := g.Commit(ctx, worktreeDir, "feature commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Meanwhile, base moves forward: push a new commit to origin/main that
	// doesn't touch feature.md.
	writeFile(t, baseRepo, "other.md", "unrelated v2\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "base moved on")
	runMust(t, baseRepo, "push", "origin", "main")

	if err := g.SyncWithBase(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("SyncWithBase: %v", err)
	}

	// Base's new content should now be visible in the worktree.
	other, err := os.ReadFile(filepath.Join(worktreeDir, "other.md"))
	if err != nil {
		t.Fatalf("read other.md: %v", err)
	}
	if string(other) != "unrelated v2\n" {
		t.Errorf("other.md not synced from base: %q", other)
	}
	// The feature branch's own commit should still be there.
	feature, err := os.ReadFile(filepath.Join(worktreeDir, "feature.md"))
	if err != nil {
		t.Fatalf("read feature.md: %v", err)
	}
	if string(feature) != "my change\n" {
		t.Errorf("feature.md lost during sync: %q", feature)
	}
	// No leftover conflict state.
	dirty, err := g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges: %v", err)
	}
	if dirty {
		t.Errorf("worktree should be clean after a conflict-free sync")
	}
}

// TestExecGit_SyncWithBase_LeavesConflictForRunner covers the case that
// motivates this method: base and the feature branch both touched the same
// file. SyncWithBase must NOT error — the merge conflict is exactly the
// state the runner (address-comment / fix-CI) is meant to resolve.
func TestExecGit_SyncWithBase_LeavesConflictForRunner(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "shared.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/conflict"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Feature branch edits shared.md.
	writeFile(t, worktreeDir, "shared.md", "feature version\n")
	if err := g.Commit(ctx, worktreeDir, "feature edit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Base also edits shared.md, differently, and gets pushed.
	writeFile(t, baseRepo, "shared.md", "base version\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "base edit")
	runMust(t, baseRepo, "push", "origin", "main")

	if err := g.SyncWithBase(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("SyncWithBase should not error on an ordinary merge conflict, got: %v", err)
	}

	// The worktree should be left with unmerged paths / conflict markers so
	// the runner can resolve them.
	dirty, err := g.HasChanges(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("HasChanges: %v", err)
	}
	if !dirty {
		t.Errorf("worktree should show conflict state as changes")
	}
	content, err := os.ReadFile(filepath.Join(worktreeDir, "shared.md"))
	if err != nil {
		t.Fatalf("read shared.md: %v", err)
	}
	if !strings.Contains(string(content), "<<<<<<<") {
		t.Errorf("shared.md should contain conflict markers, got: %q", content)
	}
}

// TestExecGit_SyncWithBase_DiscardsDirtyWorktree covers the upfront
// discard: uncommitted changes at call time (e.g. left by an interrupted
// invocation) must be thrown away before fetching/merging, even when the
// changes wouldn't overlap the merge — git itself would silently merge
// over those.
func TestExecGit_SyncWithBase_DiscardsDirtyWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/dirty"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Base moves forward without touching the file we're about to dirty,
	// so git's own merge would NOT refuse — only the upfront guard can.
	writeFile(t, baseRepo, "other.md", "base moved on\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "base moved on")
	runMust(t, baseRepo, "push", "origin", "main")

	// Leave an uncommitted change in the worktree.
	writeFile(t, worktreeDir, "wip.md", "uncommitted\n")

	if err := g.SyncWithBase(ctx, worktreeDir, "main"); err != nil {
		t.Fatalf("SyncWithBase: %v", err)
	}

	// The merge must have happened: base's new file should now be present.
	if _, err := os.Stat(filepath.Join(worktreeDir, "other.md")); err != nil {
		t.Errorf("other.md should exist — SyncWithBase must merge once the dirty tree is discarded; err=%v", err)
	}
	// The uncommitted change must be gone.
	if _, err := os.Stat(filepath.Join(worktreeDir, "wip.md")); !os.IsNotExist(err) {
		t.Errorf("wip.md should have been discarded by SyncWithBase; err=%v", err)
	}
}

// TestExecGit_SyncWithBase_FetchErrorPropagates ensures a genuine
// infrastructure failure (no such base branch) is returned as an error,
// not silently swallowed like a real conflict would be.
func TestExecGit_SyncWithBase_FetchErrorPropagates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/badbase"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	if err := g.SyncWithBase(ctx, worktreeDir, "does-not-exist"); err == nil {
		t.Errorf("SyncWithBase against a nonexistent base branch should error")
	}
}

func TestExecGit_GIT_TERMINAL_PROMPT_DisablesPrompting(t *testing.T) {
	// We can't really test that the env var is set without intercepting
	// the subprocess. Smoke-check that the runner sets it by reading it
	// back via `env`. Skipping if `env` isn't available is fine.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	g := NewExec("", "")
	// `git config --get` will fail on a missing key without error if the
	// terminal-prompt env isn't tripping anything. The real assertion is
	// just that g.run doesn't hang on missing creds — but proving "doesn't
	// hang" in a unit test isn't feasible. Instead, verify the helper's
	// env-construction is the one that includes GIT_TERMINAL_PROMPT=0.
	// (Static check via reading the source is the honest cover here; this
	// test exists as a placeholder so future changes don't drop the var.)
	if !strings.Contains(g.envProbe(), "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("ExecGit should disable terminal prompts")
	}
}

// TestExecGit_Commit_SkipsUntrackedBinaries regression-tests the case
// where a runner runs `go build` inside the worktree, the resulting
// binary lands alongside the source, and `git add -A` would otherwise
// sweep it into the commit (triggering pre-commit hooks that cap file
// size). After the fix, Commit must stage the text file, leave the
// binary out of the commit, and delete it from the worktree rather than
// leaving it to linger uncommitted.
func TestExecGit_Commit_SkipsUntrackedBinaries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/bin"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Simulate the runner's output: a source file plus a compiled binary
	// (NUL bytes in the leading 512 — `\x7fELF` is the standard Linux
	// magic, but any NUL triggers the binary heuristic).
	writeFile(t, worktreeDir, "main.go", "package main\nfunc main() {}\n")
	const binName = "app"
	if err := os.WriteFile(filepath.Join(worktreeDir, binName),
		[]byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0, 1, 0, 0, 0}, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	if err := g.Commit(ctx, worktreeDir, "add main.go"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The commit should contain main.go but NOT the binary.
	out, err := exec.Command("git", "-C", worktreeDir, "show", "--name-only", "--pretty=").Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	files := strings.Fields(string(out))
	hasMain, hasBin := false, false
	for _, f := range files {
		if f == "main.go" {
			hasMain = true
		}
		if f == binName {
			hasBin = true
		}
	}
	if !hasMain {
		t.Errorf("commit should include main.go, got %v", files)
	}
	if hasBin {
		t.Errorf("commit must NOT include the %s binary, got %v", binName, files)
	}

	// The binary should be deleted from the worktree, not left to linger
	// uncommitted — the harness owns every commit, so nothing worth
	// keeping should ever sit uncommitted between turns.
	if _, err := os.Stat(filepath.Join(worktreeDir, binName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("binary should have been deleted from worktree, stat err = %v", err)
	}
}

// TestExecGit_Commit_OnlyBinaries_ReturnsNoChanges covers the edge case
// where the runner produced nothing but binary blobs — after filtering,
// nothing is staged and Commit should report ErrNoChanges rather than
// invoke `git commit` with an empty index.
func TestExecGit_Commit_OnlyBinaries_ReturnsNoChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()
	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/only-bin"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "blob"),
		[]byte{0, 0, 0, 0, 0}, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := g.Commit(ctx, worktreeDir, "msg"); !errors.Is(err, ErrNoChanges) {
		t.Errorf("Commit with only-binary untracked: want ErrNoChanges, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, "blob")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob should have been deleted from worktree, stat err = %v", err)
	}
}

// TestExecGit_Commit_HookRejection_ReturnsHookRejectionError covers a
// target repo whose own pre-commit hook rejects the commit (ADR-0075):
// Commit should surface the hook's output via a *HookRejectionError rather
// than a bare wrapped error, so callers can distinguish it from other
// commit failures and feed the output back to the runner.
func TestExecGit_Commit_HookRejection_ReturnsHookRejectionError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	// A pre-commit hook that always rejects, explaining why on stdout —
	// mirroring how e.g. gofmt/lint hooks report their complaint. Set
	// core.hooksPath explicitly so the test is hermetic even on a machine
	// whose global git config points hooksPath elsewhere.
	hooksDir := filepath.Join(baseRepo, ".git", "hooks")
	runMust(t, baseRepo, "config", "core.hooksPath", hooksDir)
	hookPath := filepath.Join(hooksDir, "pre-commit")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho 'file not formatted: main.go'\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", "syntropy/test/hook-reject"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	writeFile(t, worktreeDir, "main.go", "package main\nfunc main() {}\n")

	err := g.Commit(ctx, worktreeDir, "add main.go")
	if err == nil {
		t.Fatal("Commit: want error from rejecting hook, got nil")
	}
	var hookErr *HookRejectionError
	if !errors.As(err, &hookErr) {
		t.Fatalf("Commit: want *HookRejectionError, got %T: %v", err, err)
	}
	if !strings.Contains(hookErr.Output, "file not formatted: main.go") {
		t.Errorf("HookRejectionError.Output = %q, want it to contain the hook's message", hookErr.Output)
	}
}

// TestExecGit_HasWorkBeyondBase covers the four cases that motivate the
// method (a porcelain-only HasChanges misses committed-but-unpushed work,
// see ADR on the correctness check): fresh worktree → false; dirty tree →
// true; self-committed work with a clean tree → true; base moved forward
// while the unit sat idle → false.
func TestExecGit_HasWorkBeyondBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "v1\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	// Case 1: fresh worktree, runner did nothing → false.
	wt1 := filepath.Join(t.TempDir(), "wt1")
	if err := g.EnsureBranch(ctx, wt1, baseRepo, "main", "syntropy/test/work-a"); err != nil {
		t.Fatalf("EnsureBranch wt1: %v", err)
	}
	got, err := g.HasWorkBeyondBase(ctx, wt1, "main")
	if err != nil {
		t.Fatalf("HasWorkBeyondBase (fresh): %v", err)
	}
	if got {
		t.Errorf("fresh worktree: want false, got true")
	}

	// Case 2: dirty tree (uncommitted change) → true.
	writeFile(t, wt1, "wip.md", "uncommitted\n")
	got, err = g.HasWorkBeyondBase(ctx, wt1, "main")
	if err != nil {
		t.Fatalf("HasWorkBeyondBase (dirty): %v", err)
	}
	if !got {
		t.Errorf("dirty tree: want true, got false")
	}

	// Case 3: the motivating case — runner committed its own work, tree is
	// clean again. Porcelain-only HasChanges says false; this must say true.
	if err := g.Commit(ctx, wt1, "self-committed work"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if dirty, _ := g.HasChanges(ctx, wt1); dirty {
		t.Fatalf("precondition: tree should be clean after commit")
	}
	got, err = g.HasWorkBeyondBase(ctx, wt1, "main")
	if err != nil {
		t.Fatalf("HasWorkBeyondBase (committed): %v", err)
	}
	if !got {
		t.Errorf("committed-but-unpushed work: want true, got false")
	}

	// Case 4: base moves forward while a second, idle worktree does nothing.
	// Comparing HEAD to origin/main directly could misread this; merge-base
	// must not count upstream's commits as the unit's work.
	wt2 := filepath.Join(t.TempDir(), "wt2")
	if err := g.EnsureBranch(ctx, wt2, baseRepo, "main", "syntropy/test/work-b"); err != nil {
		t.Fatalf("EnsureBranch wt2: %v", err)
	}
	writeFile(t, baseRepo, "README.md", "v2 — base moved on\n")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "base moved on")
	runMust(t, baseRepo, "push", "origin", "main")
	runMust(t, wt2, "fetch", "origin", "main")

	got, err = g.HasWorkBeyondBase(ctx, wt2, "main")
	if err != nil {
		t.Fatalf("HasWorkBeyondBase (upstream moved, idle): %v", err)
	}
	if got {
		t.Errorf("base moved but unit idle: want false, got true")
	}

	// wt1's committed work must still register after base moved forward.
	got, err = g.HasWorkBeyondBase(ctx, wt1, "main")
	if err != nil {
		t.Fatalf("HasWorkBeyondBase (committed, base moved): %v", err)
	}
	if !got {
		t.Errorf("committed work after base moved: want true, got false")
	}

	// A nonexistent base branch is an error, not a silent false.
	if _, err := g.HasWorkBeyondBase(ctx, wt1, "does-not-exist"); err == nil {
		t.Errorf("HasWorkBeyondBase against a nonexistent base branch should error")
	}
}

// newBaseRepoWithOrigin sets up a real on-disk repo with one commit on main
// and an "origin" remote (a bare clone of itself), mirroring the setup used
// by TestExecGit_FullLifecycle. Returns the repo dir.
func newBaseRepoWithOrigin(t *testing.T) string {
	t.Helper()
	baseRepo := t.TempDir()
	runMust(t, baseRepo, "init", "-b", "main")
	writeFile(t, baseRepo, "README.md", "hello\n")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "add", "-A")
	runMust(t, baseRepo, "-c", "user.name=test", "-c", "user.email=t@x", "commit", "-m", "initial")

	originDir := t.TempDir()
	runMust(t, ".", "clone", "--bare", baseRepo, originDir)
	runMust(t, baseRepo, "remote", "add", "origin", originDir)
	runMust(t, baseRepo, "fetch", "origin")
	return baseRepo
}

// TestExecGit_EnsureBranch_RetriesWorktreeAddAfterLockContention covers the
// case-1 scenario from ADR-0059/the resiliency spec: `worktree add -b` hits
// ref-lock contention from a sibling Run touching the shared base_repo. A
// pre-existing refs/heads/<branch>.lock file forces the first attempt to
// fail with a real git "cannot lock ref" error; the retry's sleep hook
// clears the lock (simulating the sibling releasing it), so the next
// attempt should succeed without ever having created the branch.
func TestExecGit_EnsureBranch_RetriesWorktreeAddAfterLockContention(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := newBaseRepoWithOrigin(t)
	branchName := "syntropy/test/lock-retry"
	lockPath := filepath.Join(baseRepo, ".git", "refs", "heads", branchName+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("mkdir lock parent: %v", err)
	}
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}

	orig := defaultFetchRetry
	defer func() { defaultFetchRetry = orig }()
	sleeps := 0
	defaultFetchRetry = retryConfig{
		attempts:  5,
		baseDelay: time.Millisecond,
		sleep: func(time.Duration) {
			sleeps++
			if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove lock file: %v", err)
			}
		},
	}

	g := NewExec("t", "t@x")
	ctx := t.Context()
	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", branchName); err != nil {
		t.Fatalf("EnsureBranch: want success after lock clears, got %v", err)
	}
	if sleeps == 0 {
		t.Errorf("want at least one retry sleep, got 0 — the lock-contention attempt never happened")
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, ".git")); err != nil {
		t.Fatalf("expected worktree to be checked out: %v", err)
	}
}

// TestExecGit_EnsureBranch_CleansUpOrphanedBranchBeforeRetry covers the
// other half of case 1: a previous attempt left branchName created in
// baseRepo but never attached to a worktree (e.g. the branch ref landed,
// then registering the worktree itself lost a lock race). Without cleanup,
// `worktree add -b` would fail immediately with "already exists" — not a
// lock-contention error withRetry would even retry. EnsureBranch must
// detect and delete the orphan so the attempt can succeed.
func TestExecGit_EnsureBranch_CleansUpOrphanedBranchBeforeRetry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := newBaseRepoWithOrigin(t)
	branchName := "syntropy/test/orphan-cleanup"
	// Simulate the partial-failure leftover: branch exists, no worktree.
	runMust(t, baseRepo, "branch", branchName)

	g := NewExec("t", "t@x")
	ctx := t.Context()
	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", branchName); err != nil {
		t.Fatalf("EnsureBranch: want orphaned branch cleaned up and worktree created, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktreeDir, ".git")); err != nil {
		t.Fatalf("expected worktree to be checked out: %v", err)
	}
	current, err := g.currentBranch(ctx, worktreeDir)
	if err != nil {
		t.Fatalf("currentBranch: %v", err)
	}
	if current != branchName {
		t.Errorf("worktree is on %q, want %q", current, branchName)
	}
}

// TestExecGit_EnsureBranch_WorktreeAddBoundedRetry_PersistentFailure covers
// the case where the lock never clears: EnsureBranch must stop retrying
// after cfg.attempts and surface the last attempt's lock-contention error,
// not loop forever.
func TestExecGit_EnsureBranch_WorktreeAddBoundedRetry_PersistentFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	baseRepo := newBaseRepoWithOrigin(t)
	branchName := "syntropy/test/lock-persistent"
	lockPath := filepath.Join(baseRepo, ".git", "refs", "heads", branchName+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("mkdir lock parent: %v", err)
	}
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	// Never removed — the lock persists for every attempt.

	orig := defaultFetchRetry
	defer func() { defaultFetchRetry = orig }()
	calls := 0
	defaultFetchRetry = retryConfig{
		attempts:  3,
		baseDelay: time.Millisecond,
		sleep:     func(time.Duration) { calls++ },
	}

	g := NewExec("t", "t@x")
	ctx := t.Context()
	worktreeDir := filepath.Join(t.TempDir(), "wt")
	err := g.EnsureBranch(ctx, worktreeDir, baseRepo, "main", branchName)
	if err == nil {
		t.Fatal("EnsureBranch: want error when lock never clears, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "lock") {
		t.Errorf("want the lock-contention error surfaced, got %v", err)
	}
	if calls != defaultFetchRetry.attempts-1 {
		t.Errorf("want %d retry sleeps (bounded by attempts), got %d", defaultFetchRetry.attempts-1, calls)
	}
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should not have been created, got err=%v", err)
	}
}

// TestWithRetry_SucceedsAfterTransientFailures covers the case that
// motivates ADR-0059: a fetch fails a couple of times with a retryable
// lock-contention error, then succeeds once the sibling Run releases the
// lock. withRetry must retry through the failures and return nil, and
// must back off (sleep) between attempts but not after the final success.
func TestWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	var sleeps []time.Duration
	cfg := retryConfig{
		attempts:  5,
		baseDelay: 10 * time.Millisecond,
		sleep:     func(d time.Duration) { sleeps = append(sleeps, d) },
	}

	calls := 0
	err := withRetry(t.Context(), cfg, isLockContention, func() error {
		calls++
		if calls < 3 {
			return errors.New("fatal: Unable to create '/repo/.git/refs/remotes/origin/main.lock': File exists.")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withRetry: want nil after eventual success, got %v", err)
	}
	if calls != 3 {
		t.Errorf("want 3 calls (2 failures + 1 success), got %d", calls)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if len(sleeps) != len(want) || sleeps[0] != want[0] || sleeps[1] != want[1] {
		t.Errorf("want backoff sleeps %v, got %v", want, sleeps)
	}
}

// TestWithRetry_BoundedFailure_ReturnsLastError covers the case where the
// lock never clears: withRetry must stop after cfg.attempts (not loop
// forever) and surface the final attempt's error.
func TestWithRetry_BoundedFailure_ReturnsLastError(t *testing.T) {
	cfg := retryConfig{
		attempts:  3,
		baseDelay: time.Millisecond,
		sleep:     func(time.Duration) {},
	}

	calls := 0
	err := withRetry(t.Context(), cfg, isLockContention, func() error {
		calls++
		return fmt.Errorf("could not lock config file .git/config: attempt %d", calls)
	})
	if err == nil {
		t.Fatal("withRetry: want error when every attempt fails, got nil")
	}
	if calls != cfg.attempts {
		t.Errorf("want exactly %d calls, got %d", cfg.attempts, calls)
	}
	if !strings.Contains(err.Error(), "attempt 3") {
		t.Errorf("want the last attempt's error surfaced, got %v", err)
	}
}

// TestWithRetry_NonRetryableError_ReturnsImmediately covers a genuine
// failure (bad remote, auth, etc.) that isLockContention doesn't recognize:
// withRetry must not retry it at all.
func TestWithRetry_NonRetryableError_ReturnsImmediately(t *testing.T) {
	cfg := retryConfig{
		attempts:  5,
		baseDelay: time.Millisecond,
		sleep:     func(time.Duration) { t.Fatal("should not sleep for a non-retryable error") },
	}

	calls := 0
	err := withRetry(t.Context(), cfg, isLockContention, func() error {
		calls++
		return errors.New("fatal: repository 'origin' not found")
	})
	if err == nil {
		t.Fatal("want error surfaced")
	}
	if calls != 1 {
		t.Errorf("want exactly 1 call for a non-retryable error, got %d", calls)
	}
}

// TestExecGit_IsIsolatedWorktree covers the deterministic guard used to
// verify a directory is a real linked git worktree — distinct from the
// main checkout — before granting it filesystem-write access.
func TestExecGit_IsIsolatedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// Main repo, with a linked worktree branched off it.
	mainRepo := t.TempDir()
	runMust(t, mainRepo, "init", "-b", "main")
	writeFile(t, mainRepo, "README.md", "hello\n")
	runMust(t, mainRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, mainRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	linkedDir := filepath.Join(t.TempDir(), "wt")
	runMust(t, mainRepo, "worktree", "add", "-b", "feature", linkedDir)

	// A wholly separate, unrelated repo — its own main checkout.
	unrelatedRepo := t.TempDir()
	runMust(t, unrelatedRepo, "init", "-b", "main")
	writeFile(t, unrelatedRepo, "README.md", "unrelated\n")
	runMust(t, unrelatedRepo, "-c", "user.name=t", "-c", "user.email=t@x", "add", "-A")
	runMust(t, unrelatedRepo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "initial")

	nonGitDir := t.TempDir()
	missingDir := filepath.Join(t.TempDir(), "does-not-exist")

	g := NewExec("t", "t@x")
	ctx := t.Context()

	tests := []struct {
		name    string
		dir     string
		want    bool
		wantErr bool
	}{
		{name: "linked worktree", dir: linkedDir, want: true},
		{name: "main checkout", dir: mainRepo, want: false},
		{name: "unrelated repo", dir: unrelatedRepo, want: false},
		{name: "non-git dir", dir: nonGitDir, wantErr: true},
		{name: "missing path", dir: missingDir, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := g.IsIsolatedWorktree(ctx, tt.dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got none (result=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("IsIsolatedWorktree(%s) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}
}

// --- helpers ---

func runMust(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" && dir != "." {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

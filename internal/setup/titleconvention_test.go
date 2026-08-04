package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTitleConvention_FlagWins(t *testing.T) {
	got, err := ResolveTitleConvention("Conventional Commits", true, func() (string, error) {
		t.Fatal("prompt should not be called when --title-convention is set")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Conventional Commits" {
		t.Fatalf("got %q, want flag value", got)
	}
}

func TestResolveTitleConvention_NonInteractiveWithoutFlagIsEmpty(t *testing.T) {
	got, err := ResolveTitleConvention("", false, func() (string, error) {
		t.Fatal("prompt should not be called when not interactive")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestResolveTitleConvention_InteractivePromptAnswerWins(t *testing.T) {
	got, err := ResolveTitleConvention("", true, func() (string, error) {
		return "ticket ID prefix like PROJ-123: ...", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ticket ID prefix like PROJ-123: ..." {
		t.Fatalf("got %q, want prompt answer", got)
	}
}

func TestWriteRepoConfig_SkipsWhenConventionEmpty(t *testing.T) {
	dir := t.TempDir()
	wrote, err := WriteRepoConfig(dir, "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wrote {
		t.Fatalf("expected no write for empty convention")
	}
	if _, err := os.Stat(RepoConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("expected no file written, stat err = %v", err)
	}
}

func TestWriteRepoConfig_WritesFile(t *testing.T) {
	dir := t.TempDir()
	wrote, err := WriteRepoConfig(dir, "Conventional Commits", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wrote {
		t.Fatalf("expected write to happen")
	}
	data, err := os.ReadFile(filepath.Join(dir, ".syntropy.yml"))
	if err != nil {
		t.Fatalf("read repo config: %v", err)
	}
	want := "title_convention: Conventional Commits\n"
	if string(data) != want {
		t.Fatalf("got %q, want %q", string(data), want)
	}
}

func TestWriteRepoConfig_DoesNotClobberExistingWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := RepoConfigPath(dir)
	if err := os.WriteFile(path, []byte("title_convention: existing\n"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	wrote, err := WriteRepoConfig(dir, "new convention", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wrote {
		t.Fatalf("expected no write when file exists and force is false")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repo config: %v", err)
	}
	if string(data) != "title_convention: existing\n" {
		t.Fatalf("existing file was clobbered: %q", string(data))
	}
}

func TestReadRepoConfig_AbsentFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ReadRepoConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TitleConvention != "" {
		t.Fatalf("got %q, want empty TitleConvention for absent file", cfg.TitleConvention)
	}
}

func TestReadRepoConfig_PresentConvention(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(RepoConfigPath(dir), []byte("title_convention: Conventional Commits\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cfg, err := ReadRepoConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TitleConvention != "Conventional Commits" {
		t.Fatalf("got %q, want %q", cfg.TitleConvention, "Conventional Commits")
	}
}

func TestReadRepoConfig_BlankField(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(RepoConfigPath(dir), []byte("# no convention set\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cfg, err := ReadRepoConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TitleConvention != "" {
		t.Fatalf("got %q, want empty TitleConvention for blank field", cfg.TitleConvention)
	}
}

// --- BlankSentinel semantics ---

func TestReadRepoConfig_BlankSentinel_IsConfiguredButHasNoEffectiveValue(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(RepoConfigPath(dir), []byte("title_convention: blank\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cfg, err := ReadRepoConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TitleConvention != BlankSentinel {
		t.Fatalf("got %q, want the raw sentinel %q preserved on the struct", cfg.TitleConvention, BlankSentinel)
	}
	if !cfg.IsConfigured() {
		t.Error("a field explicitly recorded as blank must count as configured — don't re-ask")
	}
	if got := cfg.EffectiveTitleConvention(); got != "" {
		t.Errorf("EffectiveTitleConvention() = %q, want empty — the sentinel must never leak as a real value", got)
	}
}

func TestRepoConfig_MissingField_IsNotConfigured(t *testing.T) {
	var cfg RepoConfig // never-asked: zero value, as if the field were absent
	if cfg.IsConfigured() {
		t.Error("an absent/never-asked field must not report as configured")
	}
	if got := cfg.EffectiveTitleConvention(); got != "" {
		t.Errorf("EffectiveTitleConvention() = %q, want empty for an unconfigured field", got)
	}
}

func TestRepoConfig_RealValue_IsConfiguredAndEffective(t *testing.T) {
	cfg := RepoConfig{TitleConvention: "Conventional Commits"}
	if !cfg.IsConfigured() {
		t.Error("a real value must count as configured")
	}
	if got := cfg.EffectiveTitleConvention(); got != "Conventional Commits" {
		t.Errorf("EffectiveTitleConvention() = %q, want the real value unchanged", got)
	}
}

func TestWriteRepoConfig_ForceOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := RepoConfigPath(dir)
	if err := os.WriteFile(path, []byte("title_convention: existing\n"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	wrote, err := WriteRepoConfig(dir, "new convention", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wrote {
		t.Fatalf("expected write with force=true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repo config: %v", err)
	}
	if string(data) != "title_convention: new convention\n" {
		t.Fatalf("got %q, want overwritten content", string(data))
	}
}

// --- SpecTool (RepoConfig field, never in MissingFields) ---

func TestResolveRepoSpecTool_FlagWins(t *testing.T) {
	got, err := ResolveRepoSpecTool("spec-kit", true, func() (string, error) {
		t.Fatal("prompt should not be called when --repo-spec-tool is set")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "spec-kit" {
		t.Fatalf("got %q, want flag value", got)
	}
}

func TestResolveRepoSpecTool_NonInteractiveWithoutFlagIsEmpty(t *testing.T) {
	got, err := ResolveRepoSpecTool("", false, func() (string, error) {
		t.Fatal("prompt should not be called when not interactive")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestResolveRepoSpecTool_InteractivePromptAnswerWins(t *testing.T) {
	got, err := ResolveRepoSpecTool("", true, func() (string, error) {
		return "spec-kit", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "spec-kit" {
		t.Fatalf("got %q, want prompt answer", got)
	}
}

func TestWriteRepoConfig_WritesSpecTool(t *testing.T) {
	dir := t.TempDir()
	wrote, err := WriteRepoConfig(dir, "", "spec-kit", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wrote {
		t.Fatalf("expected write to happen for a non-empty spec tool")
	}
	cfg, err := ReadRepoConfig(dir)
	if err != nil {
		t.Fatalf("read repo config: %v", err)
	}
	if cfg.SpecTool != "spec-kit" {
		t.Fatalf("got %q, want %q", cfg.SpecTool, "spec-kit")
	}
}

func TestMissingFields_NeverReportsSpecTool(t *testing.T) {
	got := MissingFields(RepoConfig{})
	for _, f := range got {
		if f == "spec_tool" {
			t.Fatalf("spec_tool must never appear in MissingFields, got %v", got)
		}
	}
}

// --- MissingFields ---

func TestMissingFields_UnconfiguredReportsTitleConvention(t *testing.T) {
	got := MissingFields(RepoConfig{})
	if len(got) != 1 || got[0] != "title_convention" {
		t.Fatalf("got %v, want [title_convention]", got)
	}
}

func TestMissingFields_RealValueReportsNothing(t *testing.T) {
	got := MissingFields(RepoConfig{TitleConvention: "Conventional Commits"})
	if len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestMissingFields_BlankSentinelReportsNothing(t *testing.T) {
	got := MissingFields(RepoConfig{TitleConvention: BlankSentinel})
	if len(got) != 0 {
		t.Fatalf("got %v, want none — blank counts as decided", got)
	}
}

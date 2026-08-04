package setup

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ReadRepoConfig reads `.syntropy.yml` from repoDir, returning a zero-value
// RepoConfig (no error) if the file doesn't exist — absence means "no
// convention set", not a failure. Mirrors WriteRepoConfig's file-presence
// convention (ADR-0052).
func ReadRepoConfig(repoDir string) (RepoConfig, error) {
	path := RepoConfigPath(repoDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RepoConfig{}, nil
		}
		return RepoConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg RepoConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return RepoConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// RepoConfig is the on-disk shape of `.syntropy.yml`, a per-repo (not
// per-user) config file living at the root of a spec's `base_repo`.
type RepoConfig struct {
	// TitleConvention is free-text guidance on how this repo likes its
	// PR/MR titles phrased, e.g. "Conventional Commits" or "ticket ID
	// prefix like PROJ-123: ...". Empty means the field is missing from
	// the file entirely — this repo has never been asked (e.g. an older
	// .syntropy.yml predating this field, or the file doesn't exist at
	// all) — an agent following the syntropy Skill should ask the user
	// and persist an answer. BlankSentinel means the user WAS asked and
	// deliberately chose not to set one — don't ask again, and don't
	// treat it as a convention to inject anywhere (see
	// EffectiveTitleConvention).
	TitleConvention string `yaml:"title_convention"`

	// SpecTool is a per-repo override of the global default spec tool
	// (config.Config.SpecTool, ADR-0051/increment-1) an agent should route
	// spec creation/viewing to for this repo specifically, e.g. "spec-kit".
	// Unlike TitleConvention, this is a pure optional override, not a
	// field every repo is expected to have an opinion on — it's never
	// included in MissingFields, so an agent following the syntropy Skill
	// never prompts for it as part of the per-repo setup conversation; a
	// repo with no override simply falls back to the global default.
	SpecTool string `yaml:"spec_tool,omitempty"`
}

// BlankSentinel is written to a RepoConfig field, instead of leaving it
// as an empty string or omitting it, when the user was explicitly asked
// and chose not to set a value. Plain "" is ambiguous between "never
// asked" and "asked, declined" — every field in this file needs a way to
// distinguish the two so an agent following the syntropy Skill knows
// whether to ask again, and so new fields added in a later syntropy
// version are correctly recognised as unasked (empty) even in an
// existing repo's .syntropy.yml, rather than mistaken for a deliberate
// blank.
const BlankSentinel = "blank"

// EffectiveTitleConvention returns the value to actually use when
// building a runner prompt: the real convention if one was set, or ""
// for both "never asked" and BlankSentinel — callers that gate a prompt
// block on non-empty (e.g. the runner's `if req.TitleConvention != ""`)
// should call this rather than reading TitleConvention directly, so
// BlankSentinel never leaks into prompt text verbatim.
func (c RepoConfig) EffectiveTitleConvention() string {
	if c.TitleConvention == BlankSentinel {
		return ""
	}
	return c.TitleConvention
}

// IsConfigured reports whether TitleConvention has been decided at all —
// either a real convention or an explicit BlankSentinel — as opposed to
// being absent, which means an agent following the syntropy Skill should
// ask the user for it.
func (c RepoConfig) IsConfigured() bool {
	return c.TitleConvention != ""
}

// MissingFields returns the names of every recognised RepoConfig field
// that hasn't been decided yet (see IsConfigured) — i.e. that an agent
// following the syntropy Skill should ask the user about. Pure, cheap,
// deterministic: no LLM invocation needed to answer "is this repo fully
// configured" (ADR-0083) — CheckRepoConfig/`syntropy config check` calls
// this so an agent can gate a conversational ask on its output instead of
// reading and reasoning about the YAML file itself every time.
//
// Add a case here for every new field RepoConfig grows that an agent
// should proactively ask about — this is the one place the "which fields
// exist" list needs to stay current. SpecTool is deliberately excluded:
// it's an optional per-repo override with a global fallback, not
// something every repo needs a decided answer for.
func MissingFields(cfg RepoConfig) []string {
	var missing []string
	if !cfg.IsConfigured() {
		missing = append(missing, "title_convention")
	}
	return missing
}

// RepoConfigPath returns the `.syntropy.yml` path for the given repo root.
func RepoConfigPath(repoDir string) string {
	return filepath.Join(repoDir, ".syntropy.yml")
}

// ResolveTitleConvention picks the free-text title convention `syntropy
// setup` should write to `.syntropy.yml`. Precedence: --title-convention
// flag, then (if interactive) the prompt's answer, then empty — a
// non-interactive run with no flag makes no claim about a convention
// rather than guessing one.
//
// prompt is called only when flagConvention is empty and interactive is
// true; it returns the raw line the user typed, or an error reading stdin.
func ResolveTitleConvention(flagConvention string, interactive bool, prompt func() (string, error)) (string, error) {
	if flagConvention != "" {
		return flagConvention, nil
	}
	if !interactive {
		return "", nil
	}
	answer, err := prompt()
	if err != nil {
		return "", fmt.Errorf("read title convention: %w", err)
	}
	return answer, nil
}

// WriteRepoConfig writes `.syntropy.yml` into repoDir with the given title
// convention and spec tool override. It's a no-op (returns false, nil) when
// both convention and specTool are empty — there's nothing to persist — or
// when the file already exists and force is false, so a user's local edits
// to it are never clobbered by a later `syntropy setup` run.
func WriteRepoConfig(repoDir, convention, specTool string, force bool) (bool, error) {
	if convention == "" && specTool == "" {
		return false, nil
	}
	path := RepoConfigPath(repoDir)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	data, err := yaml.Marshal(RepoConfig{TitleConvention: convention, SpecTool: specTool})
	if err != nil {
		return false, fmt.Errorf("marshal repo config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// ResolveRepoSpecTool picks the per-repo spec tool override `syntropy
// setup` should write to `.syntropy.yml`. Precedence: --repo-spec-tool
// flag, then (if interactive) the prompt's answer, then empty — a
// non-interactive run with no flag makes no claim about an override
// rather than guessing one, leaving the repo to fall back to the global
// default spec tool. Mirrors ResolveTitleConvention's persistence path.
//
// prompt is called only when flagSpecTool is empty and interactive is
// true; it returns the raw line the user typed, or an error reading stdin.
func ResolveRepoSpecTool(flagSpecTool string, interactive bool, prompt func() (string, error)) (string, error) {
	if flagSpecTool != "" {
		return flagSpecTool, nil
	}
	if !interactive {
		return "", nil
	}
	answer, err := prompt()
	if err != nil {
		return "", fmt.Errorf("read repo spec tool: %w", err)
	}
	return answer, nil
}

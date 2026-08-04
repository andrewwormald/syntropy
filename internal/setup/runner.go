package setup

import "fmt"

// ResolveRunner picks the runner `everflow setup` should persist as the
// default. flagRunner is whatever --runner was passed (empty if not set).
//
// With only one KnownRunners entry, an unset flag auto-selects it — there's
// nothing to choose between yet. A set flag is validated against
// KnownRunners so a typo (or a not-yet-supported agent name) fails loudly
// instead of getting silently persisted.
func ResolveRunner(flagRunner string) (string, error) {
	if flagRunner == "" {
		if len(KnownRunners) == 1 {
			return KnownRunners[0], nil
		}
		return "", fmt.Errorf("multiple runners registered (%v); pass --runner to choose one", KnownRunners)
	}
	for _, name := range KnownRunners {
		if name == flagRunner {
			return flagRunner, nil
		}
	}
	return "", fmt.Errorf("unknown runner %q (known: %v)", flagRunner, KnownRunners)
}

// ResolveModel picks the default model `everflow setup` should persist.
// Precedence: --model flag, then (if interactive) the prompt's answer,
// then the existing persisted value — so a non-interactive re-run (no TTY,
// no flag; e.g. from a script or cron) leaves a previously configured
// model untouched rather than clobbering it back to empty.
//
// prompt is called only when flagModel is empty and interactive is true; it
// receives the existing value to show as the default and returns the raw
// (untrimmed-by-caller) line the user typed, or an error reading stdin.
func ResolveModel(flagModel, existing string, interactive bool, prompt func(existing string) (string, error)) (string, error) {
	if flagModel != "" {
		return flagModel, nil
	}
	if !interactive {
		return existing, nil
	}
	answer, err := prompt(existing)
	if err != nil {
		return "", fmt.Errorf("read model: %w", err)
	}
	if answer == "" {
		return existing, nil
	}
	return answer, nil
}

// ResolveSpecTool picks the default spec tool `everflow setup` should
// persist. Precedence: --spec-tool flag, then (if interactive) the
// prompt's answer, then the existing persisted value — so a
// non-interactive re-run (no TTY, no flag; e.g. from a script or cron)
// leaves a previously configured spec tool untouched rather than
// clobbering it back to empty.
//
// prompt is called only when flagSpecTool is empty and interactive is
// true; it receives the existing value to show as the default and
// returns the raw (untrimmed-by-caller) line the user typed, or an
// error reading stdin.
func ResolveSpecTool(flagSpecTool, existing string, interactive bool, prompt func(existing string) (string, error)) (string, error) {
	if flagSpecTool != "" {
		return flagSpecTool, nil
	}
	if !interactive {
		return existing, nil
	}
	answer, err := prompt(existing)
	if err != nil {
		return "", fmt.Errorf("read spec tool: %w", err)
	}
	if answer == "" {
		return existing, nil
	}
	return answer, nil
}

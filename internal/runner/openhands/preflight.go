package openhands

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// pythonDependency pairs a Python package's import name with the pip/PyPI
// name README's "Enabling OpenHands" section tells a user to install.
type pythonDependency struct {
	module  string
	pipName string
}

// pythonDependencies are the packages a plain `uv tool install
// openhands-agent-server` does not pull in on its own (README's "Enabling
// OpenHands" §1) but agent-server needs at runtime.
var pythonDependencies = []pythonDependency{
	{module: "libtmux", pipName: "libtmux"},
	{module: "openhands_tools", pipName: "openhands-tools"},
}

// MissingDependencies checks agent-server's undeclared runtime dependencies
// — the system tmux binary, plus the libtmux and openhands-tools Python
// packages loaded into whichever interpreter agent-server's console-script
// shebang points at (per README's "Enabling OpenHands" §1) — so config
// check can report exactly what's missing by name instead of a real Run
// failing later with a cryptic subprocess error buried in agent-server's own
// stderr.
//
// A non-nil error means the check itself couldn't run to completion (e.g.
// r.Binary isn't resolvable on PATH at all, so agent-server's Python
// interpreter can't be found) — that's itself worth surfacing to a caller
// as "can't confirm dependencies," distinct from "confirmed missing."
func (r *Runner) MissingDependencies() ([]string, error) {
	binary := r.Binary
	if binary == "" {
		binary = "agent-server"
	}
	lookPath := r.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	checkImport := r.checkPythonImport
	if checkImport == nil {
		checkImport = runPythonImport
	}

	var missing []string
	if _, err := lookPath("tmux"); err != nil {
		missing = append(missing, "tmux (system binary; install via your OS package manager, e.g. `brew install tmux`)")
	}

	resolvedBinary, err := lookPath(binary)
	if err != nil {
		return missing, fmt.Errorf("%s not found on PATH: %w", binary, err)
	}

	python, err := shebangInterpreter(resolvedBinary, lookPath)
	if err != nil {
		return missing, fmt.Errorf("determine %s's Python interpreter: %w", binary, err)
	}

	for _, dep := range pythonDependencies {
		if err := checkImport(python, dep.module); err != nil {
			missing = append(missing, fmt.Sprintf("%s (Python package; reinstall with `uv tool install openhands-agent-server --with libtmux --with openhands-tools`)", dep.pipName))
		}
	}
	return missing, nil
}

// shebangInterpreter reads scriptPath's first line and returns the Python
// interpreter it names — the same interpreter that will actually run when
// scriptPath (agent-server's resolved console-script) is executed, so
// import-checking it reflects agent-server's own environment rather than an
// unrelated system python3. Handles both a direct interpreter path
// ("#!/path/to/venv/bin/python3") and an env-indirected one
// ("#!/usr/bin/env python3"), which uv tool install / pip can each produce
// depending on platform.
func shebangInterpreter(scriptPath string, lookPath func(string) (string, error)) (string, error) {
	f, err := os.Open(scriptPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return "", fmt.Errorf("%s: empty file", scriptPath)
	}
	line := scanner.Text()
	if !strings.HasPrefix(line, "#!") {
		return "", fmt.Errorf("%s: not a script (no #! shebang)", scriptPath)
	}

	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s: empty shebang line", scriptPath)
	}
	if filepath.Base(fields[0]) == "env" {
		if len(fields) < 2 {
			return "", fmt.Errorf("%s: env shebang names no interpreter", scriptPath)
		}
		return lookPath(fields[1])
	}
	return fields[0], nil
}

// runPythonImport is checkPythonImport's real implementation: it shells out
// to python and tries to import module, failing exactly when python itself
// or the package's Go equivalent of "import" fails (missing package, syntax
// error, etc).
func runPythonImport(python, module string) error {
	return exec.Command(python, "-c", "import "+module).Run()
}

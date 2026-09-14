package openhands

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgentServer writes a script at dir/agent-server with a shebang
// pointing at a fake Python interpreter path, so shebangInterpreter can read
// a real file without needing a real openhands-agent-server install.
func fakeAgentServer(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "agent-server")
	if err := os.WriteFile(script, []byte("#!/opt/venv/bin/python3\nrest of script\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return script
}

func TestMissingDependencies_AllPresent(t *testing.T) {
	script := fakeAgentServer(t, t.TempDir())
	r := &Runner{Binary: "agent-server"}
	r.lookPath = func(file string) (string, error) {
		if file == "agent-server" {
			return script, nil
		}
		return "/usr/bin/" + file, nil
	}
	r.checkPythonImport = func(python, module string) error {
		return nil
	}

	missing, err := r.MissingDependencies()
	if err != nil {
		t.Fatalf("MissingDependencies: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

func TestMissingDependencies_TmuxMissing(t *testing.T) {
	script := fakeAgentServer(t, t.TempDir())
	r := &Runner{Binary: "agent-server"}
	r.lookPath = func(file string) (string, error) {
		if file == "tmux" {
			return "", errors.New("not found")
		}
		if file == "agent-server" {
			return script, nil
		}
		return "/usr/bin/" + file, nil
	}
	r.checkPythonImport = func(python, module string) error {
		return nil
	}

	missing, err := r.MissingDependencies()
	if err != nil {
		t.Fatalf("MissingDependencies: %v", err)
	}
	if len(missing) != 1 || !strings.Contains(missing[0], "tmux") {
		t.Errorf("missing = %v, want a single tmux entry", missing)
	}
}

func TestMissingDependencies_PythonPackageMissing(t *testing.T) {
	script := fakeAgentServer(t, t.TempDir())
	r := &Runner{Binary: "agent-server"}
	r.lookPath = func(file string) (string, error) {
		if file == "agent-server" {
			return script, nil
		}
		return "/usr/bin/" + file, nil
	}
	r.checkPythonImport = func(python, module string) error {
		if module == "openhands_tools" {
			return errors.New("ModuleNotFoundError")
		}
		return nil
	}

	missing, err := r.MissingDependencies()
	if err != nil {
		t.Fatalf("MissingDependencies: %v", err)
	}
	if len(missing) != 1 || !strings.Contains(missing[0], "openhands-tools") {
		t.Errorf("missing = %v, want a single openhands-tools entry", missing)
	}
}

func TestMissingDependencies_AllMissing(t *testing.T) {
	script := fakeAgentServer(t, t.TempDir())
	r := &Runner{Binary: "agent-server"}
	r.lookPath = func(file string) (string, error) {
		if file == "tmux" {
			return "", errors.New("not found")
		}
		if file == "agent-server" {
			return script, nil
		}
		return "/usr/bin/" + file, nil
	}
	r.checkPythonImport = func(python, module string) error {
		return errors.New("ModuleNotFoundError")
	}

	missing, err := r.MissingDependencies()
	if err != nil {
		t.Fatalf("MissingDependencies: %v", err)
	}
	if len(missing) != 3 {
		t.Fatalf("missing = %v, want 3 entries (tmux, libtmux, openhands-tools)", missing)
	}
}

func TestMissingDependencies_BinaryNotFound(t *testing.T) {
	r := &Runner{Binary: "agent-server"}
	r.lookPath = func(file string) (string, error) {
		if file == "agent-server" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + file, nil
	}

	_, err := r.MissingDependencies()
	if err == nil {
		t.Fatal("expected an error when agent-server isn't resolvable on PATH")
	}
}

func TestMissingDependencies_DefaultsBinaryName(t *testing.T) {
	r := &Runner{}
	var lookedUp string
	r.lookPath = func(file string) (string, error) {
		if file != "tmux" {
			lookedUp = file
		}
		return "", errors.New("not found")
	}

	if _, err := r.MissingDependencies(); err == nil {
		t.Fatal("expected an error")
	}
	if lookedUp != "agent-server" {
		t.Errorf("looked up binary %q, want the default %q", lookedUp, "agent-server")
	}
}

func TestShebangInterpreter_DirectPath(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "agent-server")
	if err := os.WriteFile(script, []byte("#!/opt/venv/bin/python3\nrest of script\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	python, err := shebangInterpreter(script, nil)
	if err != nil {
		t.Fatalf("shebangInterpreter: %v", err)
	}
	if python != "/opt/venv/bin/python3" {
		t.Errorf("python = %q, want /opt/venv/bin/python3", python)
	}
}

func TestShebangInterpreter_EnvIndirected(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "agent-server")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env python3\nrest of script\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var lookedUp string
	python, err := shebangInterpreter(script, func(file string) (string, error) {
		lookedUp = file
		return "/resolved/python3", nil
	})
	if err != nil {
		t.Fatalf("shebangInterpreter: %v", err)
	}
	if lookedUp != "python3" {
		t.Errorf("looked up %q, want python3", lookedUp)
	}
	if python != "/resolved/python3" {
		t.Errorf("python = %q, want /resolved/python3", python)
	}
}

func TestShebangInterpreter_NoShebang(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "not-a-script")
	if err := os.WriteFile(script, []byte("just some binary content\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := shebangInterpreter(script, nil); err == nil {
		t.Fatal("expected an error for a file with no #! shebang")
	}
}

func TestRunPythonImport(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 available to exercise the real implementation")
	}
	if err := runPythonImport("python3", "os"); err != nil {
		t.Errorf("runPythonImport(python3, os): %v, want nil (os is stdlib)", err)
	}
	if err := runPythonImport("python3", "definitely_not_a_real_package_xyz"); err == nil {
		t.Error("runPythonImport(python3, definitely_not_a_real_package_xyz): want an error")
	}
}

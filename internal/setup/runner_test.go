package setup

import "testing"

func TestResolveRunner_AutoSelectsSingleKnownRunner(t *testing.T) {
	got, err := ResolveRunner("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "claude" {
		t.Fatalf("got %q, want %q", got, "claude")
	}
}

func TestResolveRunner_FlagMustMatchKnownRunner(t *testing.T) {
	got, err := ResolveRunner("claude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "claude" {
		t.Fatalf("got %q, want %q", got, "claude")
	}

	if _, err := ResolveRunner("qwen"); err == nil {
		t.Fatalf("expected error for unknown runner")
	}
}

func TestResolveModel_FlagWins(t *testing.T) {
	got, err := ResolveModel("claude-haiku-4-5", "old-default", true, func(string) (string, error) {
		t.Fatal("prompt should not be called when --model is set")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "claude-haiku-4-5" {
		t.Fatalf("got %q, want flag value", got)
	}
}

func TestResolveModel_NonInteractiveKeepsExisting(t *testing.T) {
	got, err := ResolveModel("", "old-default", false, func(string) (string, error) {
		t.Fatal("prompt should not be called when not interactive")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "old-default" {
		t.Fatalf("got %q, want existing value preserved", got)
	}
}

func TestResolveModel_InteractivePromptAnswerWins(t *testing.T) {
	got, err := ResolveModel("", "old-default", true, func(existing string) (string, error) {
		if existing != "old-default" {
			t.Fatalf("prompt got existing=%q, want %q", existing, "old-default")
		}
		return "new-model", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "new-model" {
		t.Fatalf("got %q, want %q", got, "new-model")
	}
}

func TestResolveModel_InteractiveBlankAnswerKeepsExisting(t *testing.T) {
	got, err := ResolveModel("", "old-default", true, func(string) (string, error) {
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "old-default" {
		t.Fatalf("got %q, want existing value preserved on blank answer", got)
	}
}

func TestResolveSpecTool_FlagWins(t *testing.T) {
	got, err := ResolveSpecTool("spec-kit", "old-default", true, func(string) (string, error) {
		t.Fatal("prompt should not be called when --spec-tool is set")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "spec-kit" {
		t.Fatalf("got %q, want flag value", got)
	}
}

func TestResolveSpecTool_NonInteractiveKeepsExisting(t *testing.T) {
	got, err := ResolveSpecTool("", "old-default", false, func(string) (string, error) {
		t.Fatal("prompt should not be called when not interactive")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "old-default" {
		t.Fatalf("got %q, want existing value preserved", got)
	}
}

func TestResolveSpecTool_InteractivePromptAnswerWins(t *testing.T) {
	got, err := ResolveSpecTool("", "old-default", true, func(existing string) (string, error) {
		if existing != "old-default" {
			t.Fatalf("prompt got existing=%q, want %q", existing, "old-default")
		}
		return "spec-kit", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "spec-kit" {
		t.Fatalf("got %q, want %q", got, "spec-kit")
	}
}

func TestResolveSpecTool_InteractiveBlankAnswerKeepsExisting(t *testing.T) {
	got, err := ResolveSpecTool("", "old-default", true, func(string) (string, error) {
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "old-default" {
		t.Fatalf("got %q, want existing value preserved on blank answer", got)
	}
}

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_MissingFile(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("expected zero value, got %+v", cfg)
	}
}

func TestSaveThenLoad_RoundTrips(t *testing.T) {
	home := t.TempDir()
	want := Config{Runner: "claude", Model: "claude-sonnet-5"}

	if err := Save(home, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(home)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".syntropy", "config.yaml")); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
}

func TestSaveThenLoad_UpdateCheckFields_RoundTrip(t *testing.T) {
	home := t.TempDir()
	checkedAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	want := Config{UpdateCheckedAt: checkedAt, UpdateLatestVersion: "v0.4.0"}

	if err := Save(home, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(home)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.UpdateCheckedAt.Equal(want.UpdateCheckedAt) {
		t.Fatalf("got UpdateCheckedAt %v, want %v", got.UpdateCheckedAt, want.UpdateCheckedAt)
	}
	if got.UpdateLatestVersion != want.UpdateLatestVersion {
		t.Fatalf("got UpdateLatestVersion %q, want %q", got.UpdateLatestVersion, want.UpdateLatestVersion)
	}
}

func TestSave_Overwrites(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, Config{Runner: "claude", Model: "old-model"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save(home, Config{Runner: "claude", Model: "new-model"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(home)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Model != "new-model" {
		t.Fatalf("expected overwritten model, got %q", got.Model)
	}
}

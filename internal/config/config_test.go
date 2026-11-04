package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func setupEnv(t *testing.T, event string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(event), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_EVENT_PATH", path)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	t.Setenv("INPUT_GITHUB_TOKEN", "ghs_x")
	t.Setenv("INPUT_API_KEY", "sk-x")
	t.Setenv("INPUT_API_BASE_URL", "https://example.com/v1/")
	t.Setenv("INPUT_MODEL", "")
	t.Setenv("INPUT_API_FORMAT", "")
	t.Setenv("INPUT_MAX_DIFF_BYTES", "")
	t.Setenv("INPUT_MAX_REQUESTS", "")
	t.Setenv("INPUT_EXCLUDE_PATHS", "docs/, *.md\n  gen/*.go ,")
}

func TestLoad(t *testing.T) {
	setupEnv(t, `{"pull_request":{"number":7,"head":{"sha":"abc123"}}}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PRNumber != 7 || c.HeadSHA != "abc123" {
		t.Fatalf("PR=%d sha=%s", c.PRNumber, c.HeadSHA)
	}
	if c.APIBaseURL != "https://example.com/v1" {
		t.Fatalf("base url = %q", c.APIBaseURL)
	}
	if c.MaxDiffBytes != 60000 || c.MaxRequests != 5 || c.Model != "gpt-4.1" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	want := []string{"docs/", "*.md", "gen/*.go"}
	if !reflect.DeepEqual(c.Exclude, want) {
		t.Fatalf("exclude = %v, want %v", c.Exclude, want)
	}
}

func TestLoadRejectsNonPREvent(t *testing.T) {
	setupEnv(t, `{"ref":"refs/heads/main"}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-PR event")
	}
}

func TestLoadRejectsUnknownFormat(t *testing.T) {
	setupEnv(t, `{"number":1}`)
	t.Setenv("INPUT_API_FORMAT", "anthropic")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unsupported api_format")
	}
}

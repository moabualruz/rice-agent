package paths

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRootPrefersExecutableBundleToConfiguredRoot(t *testing.T) {
	bundle := testRoot(t)
	configured := testRoot(t)
	getenv := testConfig(t, configured)
	exe := filepath.Join(bundle, "bin", "rice-agent")
	if got, err := rootForExecutable(exe, getenv); err != nil || got != bundle {
		t.Fatalf("rootForExecutable() = %q, %v; want %q", got, err, bundle)
	}
}

func TestRootUsesConfiguredRootWithoutExecutableBundle(t *testing.T) {
	configured := testRoot(t)
	getenv := testConfig(t, configured)
	exe := filepath.Join(t.TempDir(), "rice-agent")
	if got, err := rootForExecutable(exe, getenv); err != nil || got != configured {
		t.Fatalf("rootForExecutable() = %q, %v; want %q", got, err, configured)
	}
}

func TestRootEnvironmentOverrideWins(t *testing.T) {
	override := testRoot(t)
	bundle := testRoot(t)
	configured := testRoot(t)
	getenv := testConfig(t, configured)
	values := map[string]string{"AGENT_DRY_ROOT": override}
	base := getenv
	getenv = func(key string) string {
		if value, ok := values[key]; ok {
			return value
		}
		return base(key)
	}
	exe := filepath.Join(bundle, "bin", "rice-agent")
	if got, err := rootForExecutable(exe, getenv); err != nil || got != override {
		t.Fatalf("rootForExecutable() = %q, %v; want explicit override %q", got, err, override)
	}
}

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Marker), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func testConfig(t *testing.T, root string) func(string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	content, err := json.Marshal(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return func(key string) string {
		if key == "AGENT_DRY_CONFIG" {
			return configPath
		}
		if key == "HOME" {
			return home
		}
		return ""
	}
}

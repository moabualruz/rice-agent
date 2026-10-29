// Package paths resolves the Rice Agent root, state directory and per-host config file.
package paths

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Marker identifies a Rice Agent root (a clone or an installed bundle).
const Marker = "registry/wanted.json"

// Config is ~/.config/agent-dry/config.json. Tools read the same file with jq; keep keys stable.
type Config struct {
	// Root overrides root discovery (a clone or bundle directory).
	Root string `json:"root,omitempty"`
	// Hosts maps a short host name to how to reach it.
	Hosts map[string]Host `json:"hosts,omitempty"`
	// OpenBao is the secrets endpoint used by bao-run.
	OpenBao *OpenBao `json:"openbao,omitempty"`
	// Repos lists repository roots that audit and vendored-copy checks visit.
	Repos []string `json:"repos,omitempty"`
	// Semantic configures zvec-grep recall over the registry corpus (rice-agent index --semantic).
	Semantic *Semantic `json:"semantic,omitempty"`
}

// Semantic names the embedding model zvec-grep uses for the registry corpus index.
type Semantic struct {
	Embedding string `json:"embedding,omitempty"` // default local/potion-code-16m-v2
}

// Host is one remote machine.
type Host struct {
	SSH   string `json:"ssh"`             // user@address
	Shell string `json:"shell,omitempty"` // login shell family, for example fish or bash
	// Install is how scripts/deploy.sh releases a tag to this host: "bundle" (rice-agent install
	// --remote, for hosts without Go or GitHub access) or "clone" (fetch, fast-forward or detach,
	// and build the clone at Root). Empty means deploy skips the host.
	Install string `json:"install,omitempty"`
	// Root is the clone directory deploy uses when Install is "clone" (default ~/workspace/agent-dry).
	Root string `json:"root,omitempty"`
}

// OpenBao names the endpoint and the local access reference; never a secret value.
type OpenBao struct {
	Addr       string `json:"addr"`
	User       string `json:"user"`
	AccessFile string `json:"access_file"`
}

// Home returns the user's home directory honoring $HOME.
func Home(getenv func(string) string) string {
	if h := getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// ConfigPath is the per-host config file, overridable with AGENT_DRY_CONFIG.
func ConfigPath(getenv func(string) string) string {
	if p := getenv("AGENT_DRY_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Home(getenv), ".config", "agent-dry", "config.json")
}

// StateDir holds the index cache, miss log, job records and install receipts,
// overridable with AGENT_DRY_STATE.
func StateDir(getenv func(string) string) string {
	if p := getenv("AGENT_DRY_STATE"); p != "" {
		return p
	}
	return filepath.Join(Home(getenv), ".local", "state", "agent-dry")
}

// LoadConfig reads the config; a missing file yields an empty config.
func LoadConfig(getenv func(string) string) (*Config, error) {
	raw, err := os.ReadFile(ConfigPath(getenv))
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(getenv), err)
	}
	return &c, nil
}

// Root finds an explicit root, then the bundle containing the executable, then the configured root.
func Root(getenv func(string) string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return rootForExecutable(exe, getenv)
}

func rootForExecutable(exe string, getenv func(string) string) (string, error) {
	if p := getenv("AGENT_DRY_ROOT"); p != "" {
		return checkRoot(p)
	}
	if root := executableRoot(exe); root != "" {
		return root, nil
	}
	if c, err := LoadConfig(getenv); err == nil && c.Root != "" {
		return checkRoot(c.Root)
	}
	return "", fmt.Errorf("rice-agent root not found; set AGENT_DRY_ROOT or \"root\" in %s", ConfigPath(getenv))
}

func executableRoot(exe string) string {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, Marker)); err == nil {
			return dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}

func checkRoot(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, Marker)); err != nil {
		return "", fmt.Errorf("%s is not an rice-agent root (missing %s)", abs, Marker)
	}
	return abs, nil
}

package dist

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/paths"
)

// remoteTarget is the platform of every remote host; tests point it at the local platform so the
// bundled binary runs under the stub ssh.
var remoteTarget = struct{ goos, goarch string }{"linux", "amd64"}

// sshOptions are fixed: no prompts, a bounded connect, no tty so the payload stays byte-clean.
var sshOptions = []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}

// remoteCommand is the only text the remote login shell (possibly fish) ever parses.
const remoteCommand = "bash -s"

// remotePlan is printed by install --remote --dry-run.
type remotePlan struct {
	Host     string   `json:"host"`
	SSH      string   `json:"ssh"`
	Command  []string `json:"command"`
	Platform string   `json:"platform"`
	DryRun   bool     `json:"dry_run"`
	Steps    []string `json:"steps"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func runRemote(env *cli.Env, host string, dryRun, uninstall bool, prefix string) int {
	cfg, err := paths.LoadConfig(env.Getenv)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	h, ok := cfg.Hosts[host]
	if !ok {
		return cli.Envf(env, "install: host %q is not in hosts of %s", host, paths.ConfigPath(env.Getenv))
	}
	if h.SSH == "" || strings.HasPrefix(h.SSH, "-") || strings.ContainsAny(h.SSH, " \t\r\n'\"\\;|&$`") {
		return cli.Envf(env, "install: hosts.%s.ssh must be user@address", host)
	}
	installArgs := []string{"install"}
	if uninstall {
		installArgs = append(installArgs, "--uninstall")
	}
	if prefix != "" {
		installArgs = append(installArgs, "--prefix", prefix)
	}
	argv := append(append([]string{}, sshOptions...), h.SSH, remoteCommand)
	if dryRun {
		steps := []string{"build a " + remoteTarget.goos + "-" + remoteTarget.goarch + " bundle (agent-dry bundle)",
			"stream a Bash script and the bundle on stdin to " + remoteCommand,
			"verify size and sha256, extract into ~/.local/share/agent-dry/<version>/",
			"point ~/.local/share/agent-dry/current at it",
			"write ~/.config/agent-dry/config.json with root when absent",
			"run ~/.local/share/agent-dry/current/bin/agent-dry " + strings.Join(installArgs, " ")}
		if uninstall {
			steps = []string{"stream a Bash script on stdin to " + remoteCommand,
				"run ~/.local/share/agent-dry/current/bin/agent-dry " + strings.Join(installArgs, " ")}
		}
		plan := remotePlan{Host: host, SSH: h.SSH, Command: append([]string{"ssh"}, argv...),
			Platform: remoteTarget.goos + "-" + remoteTarget.goarch, DryRun: true, Steps: steps}
		if err := writeJSON(env.Stdout, plan); err != nil {
			return cli.Envf(env, "install: %v", err)
		}
		return cli.ExitOK
	}
	ssh, ok := lookPath("ssh", env.Getenv("PATH"))
	if !ok {
		return cli.Envf(env, "install: ssh not found on PATH")
	}
	var stdin io.Reader
	if uninstall {
		stdin = strings.NewReader(remoteUninstallScript(installArgs))
	} else {
		root, err := paths.Root(env.Getenv)
		if err != nil {
			return cli.Envf(env, "install: %v", err)
		}
		work, err := os.MkdirTemp("", "agent-dry-remote-")
		if err != nil {
			return cli.Envf(env, "install: %v", err)
		}
		defer os.RemoveAll(work)
		b, code := buildBundle(env, root, remoteTarget.goos, remoteTarget.goarch, filepath.Join(work, "bundle.tar.gz"))
		if b == nil {
			return code
		}
		if !versionPattern.MatchString(b.Version) || b.Version == "current" {
			return cli.Envf(env, "install: bundle version %q is not a valid directory name", b.Version)
		}
		f, err := os.Open(b.Path)
		if err != nil {
			return cli.Envf(env, "install: %v", err)
		}
		defer f.Close()
		fmt.Fprintf(env.Stderr, "agent-dry: install: sending bundle %s (%d bytes, sha256 %s) to %s\n", b.Version, b.Bytes, b.SHA256, host)
		stdin = io.MultiReader(strings.NewReader(remoteInstallScript(b, installArgs)), f)
	}
	cmd := exec.Command(ssh, argv...)
	cmd.Stdin = stdin
	cmd.Stdout = env.Stdout
	cmd.Stderr = env.Stderr
	err = cmd.Run()
	status := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		status = exitErr.ExitCode()
		if status < 0 {
			status = 255
		}
	default:
		return cli.Envf(env, "install: run ssh: %v", err)
	}
	fmt.Fprintf(env.Stderr, "agent-dry: install: remote %s exited %d\n", host, status)
	return status
}

// remoteInstallScript is the Bash program the remote runs as `bash -s`. Bash reads a script from a
// pipe one byte at a time, so when the last line calls the function, the function's `head -c`
// reads exactly the bundle bytes that follow the script on the same stdin.
func remoteInstallScript(b *Bundle, installArgs []string) string {
	args := make([]string, len(installArgs))
	for i, a := range installArgs {
		args[i] = shellQuote(a)
	}
	return `agent_dry_remote_install() {
  set -euo pipefail
  umask 022
  local version=` + shellQuote(b.Version) + `
  local size=` + strconv.FormatInt(b.Bytes, 10) + `
  local sum=` + shellQuote(b.SHA256) + `
  local base="$HOME/.local/share/agent-dry"
  local dest="$base/$version"
  case "$base" in *'"'*|*\\*) echo "agent-dry remote: unsupported HOME path" >&2; exit 3 ;; esac
  mkdir -p "$base"
  agent_dry_tmp="$(mktemp -d "$base/.install.XXXXXX")"
  trap 'rm -rf "$agent_dry_tmp"' EXIT
  local tmp="$agent_dry_tmp"
  head -c "$size" > "$tmp/bundle.tar.gz"
  local got
  got="$(wc -c < "$tmp/bundle.tar.gz" | tr -d ' ')"
  if [ "$got" != "$size" ]; then
    echo "agent-dry remote: payload truncated ($got of $size bytes)" >&2
    exit 3
  fi
  local actual
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp/bundle.tar.gz")"
  else
    actual="$(shasum -a 256 "$tmp/bundle.tar.gz")"
  fi
  actual="${actual%% *}"
  if [ "$actual" != "$sum" ]; then
    echo "agent-dry remote: payload sha256 mismatch" >&2
    exit 3
  fi
  mkdir "$tmp/x"
  tar -xzf "$tmp/bundle.tar.gz" -C "$tmp/x"
  if [ "$(cat "$tmp/x/VERSION")" != "$version" ] || [ ! -x "$tmp/x/bin/agent-dry" ]; then
    echo "agent-dry remote: bundle layout check failed" >&2
    exit 3
  fi
  if [ -e "$dest" ]; then
    mv "$dest" "$tmp/old"
  fi
  mv "$tmp/x" "$dest"
  if [ -d "$base/current" ] && [ ! -L "$base/current" ]; then
    echo "agent-dry remote: $base/current is a directory, not a symlink" >&2
    exit 3
  fi
  ln -sfn "$version" "$base/current"
  local cfg="$HOME/.config/agent-dry/config.json"
  if [ ! -e "$cfg" ]; then
    mkdir -p "$(dirname "$cfg")"
    printf '{\n  "root": "%s"\n}\n' "$base/current" > "$cfg"
    echo "agent-dry remote: wrote $cfg" >&2
  fi
  echo "agent-dry remote: installed $version at $dest" >&2
  set +e
  "$base/current/bin/agent-dry" ` + strings.Join(args, " ") + ` </dev/null
  return $?
}
agent_dry_remote_install; exit $?
`
}

// remoteUninstallScript runs the installed binary's uninstall; it carries no payload.
func remoteUninstallScript(installArgs []string) string {
	args := make([]string, len(installArgs))
	for i, a := range installArgs {
		args[i] = shellQuote(a)
	}
	return `set -euo pipefail
bin="$HOME/.local/share/agent-dry/current/bin/agent-dry"
if [ ! -x "$bin" ]; then
  echo "agent-dry remote: $bin is not installed" >&2
  exit 3
fi
exec "$bin" ` + strings.Join(args, " ") + ` </dev/null
`
}

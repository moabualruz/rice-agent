package dist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type remoteFixture struct {
	vars             map[string]string
	remoteHome, args string
}

// newRemoteFixture wires a stub ssh (testdata/dist/bin/ssh) that runs `bash -s` locally under a
// fake remote HOME, and builds bundles for the local platform so the bundled binary can run.
func newRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	requireCommands(t, "go", "bash")
	root := fixtureModule(t)
	local := tempDir(t)
	f := &remoteFixture{remoteHome: filepath.Join(tempDir(t), "remote-home"), args: filepath.Join(local, "ssh-args")}
	if err := os.MkdirAll(f.remoteHome, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(local, "config.json")
	writeFile(t, cfg, `{"hosts": {"fixture": {"ssh": "mkh@stub.invalid", "shell": "fish"}}}`, 0o644)
	stubs, err := filepath.Abs(filepath.Join(fixtures, "bin"))
	if err != nil {
		t.Fatal(err)
	}
	f.vars = map[string]string{"HOME": local, "AGENT_DRY_ROOT": root, "AGENT_DRY_CONFIG": cfg,
		"AGENT_DRY_STATE": filepath.Join(local, "state"), "PATH": stubs + ":" + os.Getenv("PATH")}
	t.Setenv("STUB_REMOTE_HOME", f.remoteHome)
	t.Setenv("STUB_SSH_ARGS", f.args)
	saved := remoteTarget
	goos, goarch, _ := strings.Cut(platform(), "-")
	remoteTarget.goos, remoteTarget.goarch = goos, goarch
	t.Cleanup(func() { remoteTarget = saved })
	return f
}

func TestRemoteInstallStreamsBundleAndPreservesExitStatus(t *testing.T) {
	f := newRemoteFixture(t)
	shells := []string{""}
	if _, err := os.Stat("/bin/bash"); err == nil {
		shells = append(shells, "/bin/bash") // Bash 3.2 on macOS
	}
	for i, shell := range shells {
		t.Setenv("STUB_BASH", shell)
		code, out, errb := run(t, f.vars, "install", "--remote", "fixture")
		if code != 7 || !strings.Contains(errb, "remote fixture exited 7") {
			t.Fatalf("shell %q: code %d out %q err %q", shell, code, out, errb)
		}
		if got := readFile(t, f.args); got != "-T\n-o\nBatchMode=yes\n-o\nConnectTimeout=10\nmkh@stub.invalid\nbash -s\n" {
			t.Fatalf("ssh argv: %q", got)
		}
		base := filepath.Join(f.remoteHome, ".local", "share", "agent-dry")
		current, err := os.Readlink(filepath.Join(base, "current"))
		if err != nil || current == "" || strings.Contains(current, "/") {
			t.Fatalf("current link: %q %v", current, err)
		}
		if got := strings.TrimSpace(readFile(t, filepath.Join(base, current, "VERSION"))); got != current {
			t.Fatalf("VERSION %q, current %q", got, current)
		}
		if info, err := os.Stat(filepath.Join(base, "current", "tools", "dfx-hello", "dfx-hello")); err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("tool entry: %v %v", info, err)
		}
		if got := readFile(t, filepath.Join(f.remoteHome, "install-args")); got != "install\n" {
			t.Fatalf("remote install args: %q", got)
		}
		cfg := filepath.Join(f.remoteHome, ".config", "agent-dry", "config.json")
		var c struct{ Root string }
		if err := json.Unmarshal([]byte(readFile(t, cfg)), &c); err != nil || c.Root != filepath.Join(base, "current") {
			t.Fatalf("remote config: %q %v", readFile(t, cfg), err)
		}
		if i == 0 && !strings.Contains(errb, "wrote "+cfg) {
			t.Fatalf("first run did not report the config write: %s", errb)
		}
		if i > 0 && strings.Contains(errb, "wrote "+cfg) {
			t.Fatal("existing remote config was rewritten")
		}
		if leftovers, _ := filepath.Glob(filepath.Join(base, ".install.*")); len(leftovers) != 0 {
			t.Fatalf("temporary directories left: %v", leftovers)
		}
	}

	// Uninstall carries no payload and runs the installed binary's uninstall.
	code, _, errb := run(t, f.vars, "install", "--remote", "fixture", "--uninstall", "--prefix", "/opt/x y")
	if code != 7 || readFile(t, filepath.Join(f.remoteHome, "install-args")) != "install --uninstall --prefix /opt/x y\n" {
		t.Fatalf("remote uninstall: %d %s", code, errb)
	}
}

func TestRemoteInstallDryRunAndErrors(t *testing.T) {
	f := newRemoteFixture(t)
	code, out, errb := run(t, f.vars, "install", "--remote", "fixture", "--dry-run")
	var plan remotePlan
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil || !plan.DryRun || plan.Command[len(plan.Command)-1] != "bash -s" {
		t.Fatalf("dry run: %d %q %s", code, out, errb)
	}
	if exists(f.args) {
		t.Fatal("dry run contacted ssh")
	}
	if code, _, errb := run(t, f.vars, "install", "--remote", "nowhere"); code != 3 || !strings.Contains(errb, `host "nowhere"`) {
		t.Fatalf("unknown host: %d %s", code, errb)
	}
	writeFile(t, f.vars["AGENT_DRY_CONFIG"], `{"hosts": {"evil": {"ssh": "-oProxyCommand=x"}}}`, 0o644)
	if code, _, _ := run(t, f.vars, "install", "--remote", "evil"); code != 3 {
		t.Fatalf("option-shaped ssh target accepted: %d", code)
	}
}

func TestRemoteScriptIsPlainBash(t *testing.T) {
	requireCommands(t, "bash")
	script := remoteInstallScript(&Bundle{Version: "v1.2.3", Bytes: 10, SHA256: strings.Repeat("a", 64)}, []string{"install", "--prefix", "it's"})
	if !strings.HasSuffix(script, "agent_dry_remote_install; exit $?\n") || hasDash(script) {
		t.Fatal("payload must follow the line that calls the installer")
	}
	path := filepath.Join(tempDir(t), "remote.sh")
	writeFile(t, path, script, 0o644)
	if out, err := execOutput("bash", "-n", path); err != nil {
		t.Fatalf("bash -n: %v %s", err, out)
	}
	if _, err := execOutput("shellcheck", "--version"); err == nil {
		if out, err := execOutput("shellcheck", "-s", "bash", path); err != nil {
			t.Fatalf("shellcheck: %s", out)
		}
	}
}

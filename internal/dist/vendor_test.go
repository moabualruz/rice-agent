package dist

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func vendorVars(root, home string) map[string]string {
	return map[string]string{"HOME": home, "AGENT_DRY_ROOT": root, "AGENT_DRY_CONFIG": filepath.Join(home, "config.json"),
		"PATH": "/usr/bin:/bin"}
}

func TestVendorCopiesWithChecksumsAndDetectsDrift(t *testing.T) {
	root := fixtureRoot(t)
	home := tempDir(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	vars := vendorVars(root, home)

	code, out, errb := run(t, vars, "vendor", "dfx-hello", "--into", repo, "--dry-run")
	if code != 0 || !strings.Contains(out, "would write tools/dfx-hello/dfx-hello") || exists(filepath.Join(repo, ".agent-dry")) {
		t.Fatalf("dry run: %d %q %s", code, out, errb)
	}
	code, out, errb = run(t, vars, "vendor", "dfx-hello", "--into", repo)
	if code != 0 || out != "tools/dfx-hello/dfx-hello\n.agent-dry/vendor.json\n.agent-dry/SHA256SUMS\n" {
		t.Fatalf("vendor: %d %q %s", code, out, errb)
	}
	code, out, errb = run(t, vars, "vendor", "--into", repo, "--as", "scripts/check.sh", "dfx-active")
	if code != 0 || out != "scripts/check.sh\n.agent-dry/vendor.json\n.agent-dry/SHA256SUMS\n" {
		t.Fatalf("vendor --as: %d %q %s", code, out, errb)
	}
	info, err := os.Stat(filepath.Join(repo, "scripts", "check.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode not preserved: %v %v", info, err)
	}
	if got, want := readFile(t, filepath.Join(repo, "scripts", "check.sh")), readFile(t, filepath.Join(root, "tools", "dfx-active", "dfx-active")); got != want {
		t.Fatal("vendored content differs")
	}
	var m VendorManifest
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(repo, ".agent-dry", "vendor.json"))), &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != "vendor/v1" || m.Source != "moabualruz/agent-dry" || !strings.HasPrefix(m.Ref, "bundle-") || len(m.Files) != 2 ||
		m.Files[0].Path != "scripts/check.sh" || m.Files[0].Tool != "dfx-active" || m.Files[1].Path != "tools/dfx-hello/dfx-hello" {
		t.Fatalf("vendor.json: %+v", m)
	}
	sums := readFile(t, filepath.Join(repo, ".agent-dry", "SHA256SUMS"))
	if sums != m.Files[0].SHA256+"  scripts/check.sh\n"+m.Files[1].SHA256+"  tools/dfx-hello/dfx-hello\n" {
		t.Fatalf("SHA256SUMS: %q", sums)
	}
	verified := 0
	for _, argv := range [][]string{{"shasum", "-a", "256", "-c", ".agent-dry/SHA256SUMS"}, {"sha256sum", "-c", ".agent-dry/SHA256SUMS"}} {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = repo
		if outb, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, outb)
		}
		verified++
	}
	if verified == 0 {
		t.Log("neither shasum nor sha256sum available; external checksum verification skipped")
	}

	if code, out, _ := run(t, vars, "vendor", "dfx-hello", "--into", repo); code != 0 || out != "vendor: up to date\n" {
		t.Fatalf("idempotent vendor: %d %q", code, out)
	}
	if code, out, _ := run(t, vars, "vendor", "--check", repo); code != 0 || !strings.Contains(out, "vendor check: 2 file(s) clean") {
		t.Fatalf("clean check: %d %q", code, out)
	}

	// Drift: the tool moved on in the root.
	entry := filepath.Join(root, "tools", "dfx-hello", "dfx-hello")
	writeFile(t, entry, readFile(t, entry)+"# newer\n", 0o755)
	newSum, _ := sha256File(entry)
	code, out, _ = run(t, vars, "vendor", "--check", repo)
	if code != 1 || !strings.Contains(out, "outdated tools/dfx-hello/dfx-hello (dfx-hello): vendored "+m.Files[1].SHA256+" current "+newSum) {
		t.Fatalf("drift: %d %q", code, out)
	}
	if !strings.Contains(out, "ok scripts/check.sh (dfx-active)") {
		t.Fatalf("drift report lost the clean file: %q", out)
	}

	// Integrity: the vendored copy was edited in place.
	writeFile(t, filepath.Join(repo, "scripts", "check.sh"), "#!/bin/sh\necho edited\n", 0o755)
	code, out, _ = run(t, vars, "vendor", "--check", repo)
	if code != 1 || !strings.Contains(out, "integrity scripts/check.sh: content does not match SHA256SUMS") {
		t.Fatalf("integrity: %d %q", code, out)
	}

	// Re-vendoring reconciles both.
	run(t, vars, "vendor", "dfx-hello", "--into", repo)
	run(t, vars, "vendor", "dfx-active", "--into", repo, "--as", "scripts/check.sh")
	if code, out, _ := run(t, vars, "vendor", "--check", repo); code != 0 {
		t.Fatalf("after reconcile: %d %q", code, out)
	}
}

func TestVendorRefusals(t *testing.T) {
	root := fixtureRoot(t)
	home := tempDir(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	vars := vendorVars(root, home)
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"vendor", "no-such-tool", "--into", repo}, 1},
		{[]string{"vendor", "dfx-deprecated", "--into", repo}, 1},
		{[]string{"vendor", "dfx-hello", "--into", repo, "--as", "../escape.sh"}, 2},
		{[]string{"vendor", "dfx-hello", "--into", repo, "--as", "/abs/escape.sh"}, 2},
		{[]string{"vendor", "dfx-hello", "--into", repo, "--as", ".agent-dry/x"}, 2},
		{[]string{"vendor", "dfx-hello", "--into", repo, "--as", ".git/hooks/x"}, 2},
		{[]string{"vendor", "dfx-hello"}, 2},
		{[]string{"vendor", "--check", repo, "dfx-hello"}, 2},
		{[]string{"vendor", "dfx-hello", "--into", filepath.Join(home, "missing")}, 2},
		{[]string{"vendor", "--check", repo}, 1},
	}
	for _, c := range cases {
		if code, _, errb := run(t, vars, c.args...); code != c.code {
			t.Errorf("%v: code %d, want %d (%s)", c.args, code, c.code, errb)
		}
	}
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "scripts")); err != nil {
		t.Fatal(err)
	}
	code, _, errb := run(t, vars, "vendor", "dfx-hello", "--into", repo, "--as", "scripts/x.sh")
	if code != 1 || !strings.Contains(errb, "is a symlink") || exists(filepath.Join(outside, "x.sh")) {
		t.Fatalf("symlinked directory: %d %s", code, errb)
	}
	if exists(filepath.Join(repo, ".agent-dry")) {
		t.Fatal("refusals wrote vendor metadata")
	}
}

func TestCheckVendoredMissingSums(t *testing.T) {
	root := fixtureRoot(t)
	home := tempDir(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	vars := vendorVars(root, home)
	if code, _, errb := run(t, vars, "vendor", "dfx-hello", "--into", repo); code != 0 {
		t.Fatal(errb)
	}
	if err := os.Remove(filepath.Join(repo, ".agent-dry", "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}
	rep, err := CheckVendored(root, repo)
	if err != nil || rep.Clean() {
		t.Fatalf("missing SHA256SUMS accepted: %+v %v", rep, err)
	}
}

package ghworkflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const safe = `name: CI
on: [push, pull_request]
permissions:
  contents: read
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
`

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// TestRepositoryWorkflows holds every workflow and local action in this repository to the policy.
func TestRepositoryWorkflows(t *testing.T) {
	root := repoRoot(t)
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range Check(f, data) {
			t.Errorf("%s", v)
		}
	}
}

func TestSafeWorkflowPasses(t *testing.T) {
	if v := Check(".github/workflows/ok.yml", []byte(safe)); len(v) != 0 {
		t.Fatalf("safe workflow flagged: %v", v)
	}
	for _, label := range []string{"ubuntu-24.04", "ubuntu-22.04-arm", "macos-14", "windows-2025"} {
		doc := strings.Replace(safe, "ubuntu-latest", label, 1)
		if v := Check("w.yml", []byte(doc)); len(v) != 0 {
			t.Fatalf("%s flagged: %v", label, v)
		}
	}
}

func TestUnsafeWorkflowsAreRefused(t *testing.T) {
	cases := map[string]string{
		"flow-style job with expression runner": "on: push\npermissions: {contents: read}\njobs:\n  a: {runs-on: \"${{ vars.R }}\", steps: []}\n",
		"quoted runs-on key self-hosted":        "on: push\npermissions: {contents: read}\njobs:\n  a:\n    \"runs-on\": [self-hosted, linux]\n",
		"custom label not in allowlist":         "on: push\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-mkh\n",
		"runs-on map form":                      "on: push\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: {group: pool}\n",
		"reusable workflow":                     "on: push\npermissions: {contents: read}\njobs:\n  a:\n    uses: evil/repo/.github/workflows/x.yml@main\n",
		"uppercase secrets context":             "on: push\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ SECRETS.TOKEN }}\n",
		"secrets inherit":                       "on: push\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    secrets: inherit\n",
		"pull_request_target list":              "on: [push, pull_request_target]\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"pull_request_target mixed case map":    "on:\n  Pull_Request_Target:\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"workflow_run":                          "on: {workflow_run: {workflows: [CI]}}\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"issue_comment":                         "on: issue_comment\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"missing permissions":                   "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"write permission":                      "on: push\npermissions:\n  contents: write\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"write-all permission":                  "on: push\npermissions: write-all\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"job level write permission":            "on: push\npermissions: {contents: read}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    permissions: {pull-requests: write}\n",
		"unparsable yaml":                       "on: [push\njobs: {",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if v := Check("w.yml", []byte(doc)); len(v) == 0 {
				t.Fatalf("not refused:\n%s", doc)
			}
		})
	}
}

func TestJobLevelPermissionsSatisfyThePolicy(t *testing.T) {
	doc := "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    permissions: {contents: read}\n  b:\n    runs-on: ubuntu-latest\n    permissions: read-all\n"
	if v := Check("w.yml", []byte(doc)); len(v) != 0 {
		t.Fatalf("flagged: %v", v)
	}
	partial := "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    permissions: {contents: read}\n  b:\n    runs-on: ubuntu-latest\n"
	if v := Check("w.yml", []byte(partial)); len(v) == 0 {
		t.Fatal("a job without permissions was accepted")
	}
}

func TestLocalActionsAreScanned(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "actions", "deep", "setup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action.yml"), []byte("runs:\n  using: composite\n  steps:\n    - run: echo ${{ secrets.X }}\n      shell: bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Files(root)
	if err != nil || len(files) != 1 {
		t.Fatalf("files %v err %v", files, err)
	}
	data, _ := os.ReadFile(files[0])
	if v := Check(files[0], data); len(v) == 0 {
		t.Fatal("secrets in a local action were not refused")
	}
}

package dist

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moabualruz/rice-agent/internal/cli"
)

func runInstructionWriteTest(t *testing.T, plan instructionWritePlan) (int, string, string) {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	command, ok := cli.Lookup("instruction")
	if !ok {
		t.Fatal("instruction command is not registered")
	}
	var out, errb bytes.Buffer
	code := command.Run(&cli.Env{Args: []string{"write", "--plan", "-"}, Stdin: bytes.NewReader(data), Stdout: &out, Stderr: &errb,
		Getenv: func(string) string { return "" }})
	return code, out.String(), errb.String()
}

func TestInstructionWriteAppliesPinnedFilesAndRepeats(t *testing.T) {
	root := tempDir(t)
	path := filepath.Join(root, "project", ".claude", "rules", "policy.md")
	content := "Owner heading.\nmanaged instruction\n"
	plan := instructionWritePlan{Schema: "rice-agent/instruction-write/v1", Files: []instructionWriteFile{{
		Root: root, Path: "project/.claude/rules/policy.md", Content: content, Mode: 0o600,
	}}}
	code, out, errb := runInstructionWriteTest(t, plan)
	if code != cli.ExitOK {
		t.Fatalf("write: %d %s", code, errb)
	}
	if got := readFile(t, path); got != content {
		t.Fatalf("content = %q", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err = %v", info, err)
	}
	var receipt []instructionWriteReceipt
	if err := json.Unmarshal([]byte(out), &receipt); err != nil || len(receipt) != 1 || receipt[0].Action != "written" {
		t.Fatalf("receipt = %q, err = %v", out, err)
	}
	before := sha256Bytes([]byte(content))
	plan.Files[0].BeforeSHA256 = &before
	code, out, errb = runInstructionWriteTest(t, plan)
	if code != cli.ExitOK || !strings.Contains(out, `"action":"unchanged"`) {
		t.Fatalf("repeat: %d %s %s", code, out, errb)
	}
}

func TestInstructionWriteRejectsChangedAndEscapingTargetsBeforeAnyWrite(t *testing.T) {
	root := tempDir(t)
	changed := filepath.Join(root, "changed.md")
	writeFile(t, changed, "owner edit\n", 0o600)
	outside := filepath.Join(tempDir(t), "outside.md")
	writeFile(t, outside, "outside\n", 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	created := "new instruction\n"
	wrong := sha256Bytes([]byte("old owner content\n"))
	plan := instructionWritePlan{Schema: "rice-agent/instruction-write/v1", Files: []instructionWriteFile{
		{Root: root, Path: "new.md", Content: created, Mode: 0o600},
		{Root: root, Path: "changed.md", BeforeSHA256: &wrong, Content: "replaced\n", Mode: 0o600},
	}}
	code, _, errb := runInstructionWriteTest(t, plan)
	if code != cli.ExitEnv || !strings.Contains(errb, "nothing changed") {
		t.Fatalf("changed snapshot: %d %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(root, "new.md")); !os.IsNotExist(err) {
		t.Fatalf("earlier target was written: %v", err)
	}
	if got := readFile(t, changed); got != "owner edit\n" {
		t.Fatalf("owner edit changed: %q", got)
	}
	plan.Files = []instructionWriteFile{{Root: root, Path: "link.md", Content: created, Mode: 0o600}}
	code, _, errb = runInstructionWriteTest(t, plan)
	if code != cli.ExitEnv || !strings.Contains(errb, "symlink") {
		t.Fatalf("symlink target: %d %s", code, errb)
	}
	if got := readFile(t, outside); got != "outside\n" {
		t.Fatalf("outside target changed: %q", got)
	}
}

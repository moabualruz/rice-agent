package audit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/moabualruz/rice-agent/internal/manifest"
)

// TestCensusCountsTrackedFilesOnly keeps untracked agent working copies out of the copy census.
func TestCensusCountsTrackedFilesOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	toolDir := filepath.Join(t.TempDir(), "check-thing")
	write(t, filepath.Join(toolDir, "check-thing"), "canonical\n")
	tool := &manifest.Tool{Name: "check-thing", Entry: "check-thing", Language: "bash", Dir: toolDir}

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	write(t, filepath.Join(repo, "scripts", "check-thing.sh"), "tracked variant\n")
	write(t, filepath.Join(repo, "testdata", "check-thing.sh"), "fixture copy\n")
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	write(t, filepath.Join(repo, ".scratch", "worktree", "scripts", "check-thing.sh"), "untracked working copy\n")

	got := censusCopies([]*manifest.Tool{tool}, []string{repo}, "test")
	if len(got.Tools) != 1 || got.Tools[0].Copies != 1 || got.Tools[0].Variants != 1 {
		t.Fatalf("want 1 tracked copy outside testdata, got %+v", got.Tools)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
}

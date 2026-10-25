package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRound6ReleaseLauncherUsesPlatformBinaryWithOrWithoutGo(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go is required to build the release fixture")
	}
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
	default:
		t.Skipf("unsupported release host %s", runtime.GOOS)
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		t.Skip("the Bash launcher is not supported on Windows")
	}
	sourceDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Match the release tool layout without the source module or internal packages.
	bundle := filepath.Join(t.TempDir(), "tools", "agent-state")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher, err := os.ReadFile(filepath.Join(sourceDir, "agent-state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "agent-state"), launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryDir := filepath.Join(bundle, "bin", platform)
	if err := os.MkdirAll(binaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binaryDir, "agent-state")
	build := exec.Command(goBinary, "build", "-o", binary, ".")
	build.Dir = sourceDir
	build.Env = make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GOFLAGS=") {
			build.Env = append(build.Env, item)
		}
	}
	build.Env = append(build.Env, "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build release binary: %v: %s", err, out)
	}

	for _, withGo := range []bool{true, false} {
		name := "without-go"
		if withGo {
			name = "with-go"
		}
		t.Run(name, func(t *testing.T) {
			path := t.TempDir()
			for _, tool := range []string{"bash", "dirname", "readlink", "uname", "mkdir", "mv", "rm"} {
				target, err := exec.LookPath(tool)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(path, tool)); err != nil {
					t.Fatal(err)
				}
			}
			if withGo {
				if err := os.Symlink(goBinary, filepath.Join(path, "go")); err != nil {
					t.Fatal(err)
				}
			}
			cache := filepath.Join(t.TempDir(), "cache")
			env := make([]string, 0, len(os.Environ())+2)
			for _, item := range os.Environ() {
				if !strings.HasPrefix(item, "PATH=") && !strings.HasPrefix(item, "XDG_CACHE_HOME=") {
					env = append(env, item)
				}
			}
			env = append(env, "PATH="+path, "XDG_CACHE_HOME="+cache)
			for _, args := range [][]string{{"--help"}, {"show", "--help"}} {
				expected, err := exec.Command(binary, args...).CombinedOutput()
				if err != nil {
					t.Fatalf("packaged binary %v failed: %v: %s", args, err, expected)
				}
				cmd := exec.Command(filepath.Join(bundle, "agent-state"), args...)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("launcher %v failed: %v: %s", args, err, out)
				}
				if !strings.Contains(string(out), "Usage: agent-state ") || string(out) != string(expected) {
					t.Fatalf("launcher %v did not use packaged platform help: %s", args, out)
				}
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("launcher attempted a source build instead of the packaged binary: %v", err)
			}
		})
	}
}

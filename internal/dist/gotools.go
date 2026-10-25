package dist

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/moabualruz/agent-dry/internal/manifest"
)

type goToolBinary struct {
	Name     string
	Path     string
	Platform string
}

func listedGoTools(root, platform string) ([]*manifest.Tool, error) {
	tools, err := manifest.LoadAll(filepath.Join(root, "tools"))
	if err != nil {
		return nil, err
	}
	var selected []*manifest.Tool
	for _, tool := range tools {
		if tool.Language != "go" || !tool.Listed() || tool.Scope != "global" {
			continue
		}
		for _, supported := range tool.Platforms {
			if supported == platform {
				selected = append(selected, tool)
				break
			}
		}
	}
	return selected, nil
}

func goToolEnvironment(goos, goarch string) []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "GOFLAGS", "GOOS", "GOARCH", "CGO_ENABLED":
			continue
		}
		env = append(env, item)
	}
	return append(env, "GOFLAGS=", "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
}

// buildGoTools produces platform binaries outside the source tree for install and bundle.
func goToolDestinations(root, outputRoot, goos, goarch string) (map[string]string, error) {
	platform := goos + "-" + goarch
	tools, err := listedGoTools(root, platform)
	if err != nil {
		return nil, err
	}
	destinations := map[string]string{}
	for _, tool := range tools {
		destinations[tool.Name] = filepath.Join(outputRoot, tool.Name, platform, tool.Name)
	}
	return destinations, nil
}

func buildGoTools(root, outputRoot, goos, goarch, pathEnv string) ([]goToolBinary, error) {
	platform := goos + "-" + goarch
	goTools, err := listedGoTools(root, platform)
	if err != nil {
		return nil, err
	}
	if len(goTools) == 0 {
		return nil, nil
	}
	goBin, hasGo := lookPath("go", pathEnv)
	out := make([]goToolBinary, 0, len(goTools))
	for _, tool := range goTools {
		target := filepath.Join(outputRoot, tool.Name, platform, tool.Name)
		if !hasGo {
			packaged := filepath.Join(tool.Dir, "bin", platform, tool.Name)
			info, err := os.Stat(packaged)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				return nil, fmt.Errorf("Go tool %s has no compiler or packaged %s binary", tool.Name, platform)
			}
			out = append(out, goToolBinary{Name: tool.Name, Path: packaged, Platform: platform})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(filepath.Dir(target), ".agent-dry-go-*")
		if err != nil {
			return nil, err
		}
		tmpName := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags", "-s -w", "-o", tmpName, ".")
		cmd.Dir = tool.Dir
		cmd.Env = goToolEnvironment(goos, goarch)
		if output, err := cmd.CombinedOutput(); err != nil {
			_ = os.Remove(tmpName)
			return nil, fmt.Errorf("build Go tool %s for %s failed: %s", tool.Name, platform, tailLines(string(output), 20))
		}
		if err := os.Chmod(tmpName, 0o755); err != nil {
			_ = os.Remove(tmpName)
			return nil, err
		}
		if err := os.Rename(tmpName, target); err != nil {
			_ = os.Remove(tmpName)
			return nil, err
		}
		out = append(out, goToolBinary{Name: tool.Name, Path: target, Platform: platform})
	}
	return out, nil
}

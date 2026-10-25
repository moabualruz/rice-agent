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
	Target   string
	Platform string
	SHA256   string
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

func goToolTarget(outputRoot, name, platform string) string {
	return filepath.Join(outputRoot, name, platform, name)
}

func goToolDestinations(root, outputRoot, goos, goarch string) (map[string]string, error) {
	platform := goos + "-" + goarch
	tools, err := listedGoTools(root, platform)
	if err != nil {
		return nil, err
	}
	destinations := map[string]string{}
	for _, tool := range tools {
		destinations[tool.Name] = goToolTarget(outputRoot, tool.Name, platform)
	}
	return destinations, nil
}

// buildGoTools produces platform binaries outside the source tree for install and bundle.
func buildGoTools(root, outputRoot, goos, goarch, pathEnv string) ([]goToolBinary, error) {
	platform := goos + "-" + goarch
	goTools, err := listedGoTools(root, platform)
	if err != nil {
		return nil, err
	}
	if len(goTools) == 0 {
		return nil, nil
	}
	for _, tool := range goTools {
		target := goToolTarget(outputRoot, tool.Name, platform)
		if err := rejectGoToolSymlinks(outputRoot, target); err != nil {
			return nil, err
		}
		info, err := os.Lstat(target)
		if err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			return nil, fmt.Errorf("Go tool target %s is not a regular file", target)
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	goBin, hasGo := lookPath("go", pathEnv)
	out := make([]goToolBinary, 0, len(goTools))
	success := false
	defer func() {
		if !success {
			discardGoTools(out)
		}
	}()
	for _, tool := range goTools {
		target := goToolTarget(outputRoot, tool.Name, platform)
		packaged := filepath.Join(tool.Dir, "bin", platform, tool.Name)
		if err := rejectGoToolSymlinks(tool.Dir, packaged); err != nil {
			return nil, err
		}
		if info, statErr := os.Lstat(packaged); statErr == nil {
			if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				return nil, fmt.Errorf("packaged Go tool %s is not an executable regular file", tool.Name)
			}
			staged, err := stageGoTool(packaged)
			if err != nil {
				return nil, err
			}
			out = append(out, goToolBinary{Name: tool.Name, Path: staged, Target: target, Platform: platform})
			continue
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		}
		if !hasGo {
			return nil, fmt.Errorf("Go tool %s has no compiler or verified packaged %s binary", tool.Name, platform)
		}
		staged, err := os.CreateTemp("", "agent-dry-go-*")
		if err != nil {
			return nil, err
		}
		stagedName := staged.Name()
		if err := staged.Close(); err != nil {
			_ = os.Remove(stagedName)
			return nil, err
		}
		cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags", "-s -w", "-o", stagedName, ".")
		cmd.Dir = tool.Dir
		cmd.Env = goToolEnvironment(goos, goarch)
		if output, err := cmd.CombinedOutput(); err != nil {
			_ = os.Remove(stagedName)
			return nil, fmt.Errorf("build Go tool %s for %s failed: %s", tool.Name, platform, tailLines(string(output), 20))
		}
		if err := os.Chmod(stagedName, 0o755); err != nil {
			_ = os.Remove(stagedName)
			return nil, err
		}
		if _, err := sha256File(stagedName); err != nil {
			_ = os.Remove(stagedName)
			return nil, err
		}
		out = append(out, goToolBinary{Name: tool.Name, Path: stagedName, Target: target, Platform: platform})
	}
	for i := range out {
		sum, err := sha256File(out[i].Path)
		if err != nil {
			return nil, err
		}
		out[i].SHA256 = sum
	}
	success = true
	return out, nil
}

func stageGoTool(source string) (string, error) {
	sourceHash, err := sha256File(source)
	if err != nil {
		return "", err
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	staged, err := os.CreateTemp("", "agent-dry-go-*")
	if err != nil {
		return "", err
	}
	name := staged.Name()
	fail := func(err error) (string, error) {
		_ = staged.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := input.Seek(0, 0); err != nil {
		return fail(err)
	}
	if _, err := staged.ReadFrom(input); err != nil {
		return fail(err)
	}
	if err := staged.Chmod(0o755); err != nil {
		return fail(err)
	}
	if err := staged.Sync(); err != nil {
		return fail(err)
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	stagedHash, err := sha256File(name)
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if stagedHash != sourceHash {
		_ = os.Remove(name)
		return "", fmt.Errorf("packaged Go tool changed while staging: %s", source)
	}
	return name, nil
}

func commitGoTools(tools []goToolBinary) error {
	type preparedFile struct{ staged, target string }
	prepared := make([]preparedFile, 0, len(tools))
	cleanup := func() {
		for _, file := range prepared {
			_ = os.Remove(file.staged)
		}
	}
	for _, tool := range tools {
		if err := os.MkdirAll(filepath.Dir(tool.Target), 0o700); err != nil {
			cleanup()
			return err
		}
		src, err := os.Open(tool.Path)
		if err != nil {
			cleanup()
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(tool.Target), ".agent-dry-go-*")
		if err != nil {
			_ = src.Close()
			cleanup()
			return err
		}
		tmpName := tmp.Name()
		prepared = append(prepared, preparedFile{staged: tmpName, target: tool.Target})
		_, copyErr := tmp.ReadFrom(src)
		srcErr := src.Close()
		if copyErr != nil {
			_ = tmp.Close()
			cleanup()
			return copyErr
		}
		if srcErr != nil {
			_ = tmp.Close()
			cleanup()
			return srcErr
		}
		if err := tmp.Chmod(0o755); err != nil {
			_ = tmp.Close()
			cleanup()
			return err
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			cleanup()
			return err
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return err
		}
		if sum, err := sha256File(tmpName); err != nil || sum != tool.SHA256 {
			cleanup()
			return fmt.Errorf("Go tool %s failed staged integrity verification", tool.Name)
		}
	}
	for _, file := range prepared {
		if err := os.Rename(file.staged, file.target); err != nil {
			cleanup()
			return err
		}
	}
	discardGoTools(tools)
	return nil
}

func rejectGoToolSymlinks(root, target string) error {
	if !within(root, target) {
		return fmt.Errorf("Go tool path is outside its root")
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	current := root
	parts := append([]string{"."}, strings.Split(rel, string(filepath.Separator))...)
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Go tool path traverses a symlink: %s", current)
		}
	}
	return nil
}

func discardGoTools(tools []goToolBinary) {
	for _, tool := range tools {
		_ = os.Remove(tool.Path)
	}
}

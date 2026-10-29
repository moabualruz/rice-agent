package dist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moabualruz/rice-agent/internal/cli"
)

const instructionWriteUsage = `Usage: rice-agent instruction write --plan -

Apply a bounded instruction write plan from stdin. Each file names an existing canonical root,
a root-relative path, the expected prior SHA-256 (null when absent), exact content, and mode.
All files are preflighted before any write. Symlinks, changed snapshots, path escapes, and owner
collisions are refused. Writes are atomic per file.

Plan schema: rice-agent/instruction-write/v1
Exit codes: 0 applied, 1 refused, 2 usage, 3 environment.
`

const maxInstructionPlanBytes = 32 << 20
const maxInstructionFileBytes = 4 << 20
const maxInstructionPlanFiles = 512

type instructionWritePlan struct {
	Schema string                 `json:"schema"`
	Files  []instructionWriteFile `json:"files"`
}

type instructionWriteFile struct {
	Root         string  `json:"root"`
	Path         string  `json:"path"`
	BeforeSHA256 *string `json:"before_sha256"`
	Content      string  `json:"content"`
	Mode         uint32  `json:"mode"`
}

type instructionWriteTarget struct {
	path, content string
	before        *string
	mode          fs.FileMode
	sha256        string
}

type instructionWriteReceipt struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	SHA256 string `json:"sha256"`
}

func runInstructionWrite(env *cli.Env) int {
	fs := flag.NewFlagSet("instruction write", flag.ContinueOnError)
	planPath := fs.String("plan", "", "")
	sub := *env
	sub.Args = env.Args[1:]
	pos, code, ok := parseArgs(&sub, fs, instructionWriteUsage)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return cli.Usagef(env, "instruction write: unexpected argument %q", pos[0])
	}
	if *planPath != "-" {
		return cli.Usagef(env, "instruction write: --plan - is required")
	}
	data, err := io.ReadAll(io.LimitReader(env.Stdin, maxInstructionPlanBytes+1))
	if err != nil {
		return cli.Envf(env, "instruction write: read plan: %v", err)
	}
	if len(data) > maxInstructionPlanBytes {
		return cli.Usagef(env, "instruction write: plan exceeds size limit")
	}
	var plan instructionWritePlan
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return cli.Usagef(env, "instruction write: invalid plan: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return cli.Usagef(env, "instruction write: plan must contain exactly one JSON value")
	}
	if plan.Schema != "rice-agent/instruction-write/v1" || len(plan.Files) == 0 || len(plan.Files) > maxInstructionPlanFiles {
		return cli.Usagef(env, "instruction write: invalid plan schema or file count")
	}
	targets := make([]instructionWriteTarget, 0, len(plan.Files))
	seen := make(map[string]bool, len(plan.Files))
	for _, file := range plan.Files {
		target, err := preflightInstructionWrite(file)
		if err != nil {
			return cli.Envf(env, "instruction write: %v; nothing changed", err)
		}
		if seen[target.path] {
			return cli.Usagef(env, "instruction write: duplicate target %s", target.path)
		}
		seen[target.path] = true
		targets = append(targets, target)
	}
	for _, target := range targets {
		if target.before != nil && target.sha256 == *target.before {
			continue
		}
		if err := verifyInstructionSnapshot(target); err != nil {
			return cli.Envf(env, "instruction write: %v; later files were not written", err)
		}
		if err := os.MkdirAll(filepath.Dir(target.path), 0o755); err != nil {
			return cli.Envf(env, "instruction write: create parent: %v", err)
		}
		if err := verifyInstructionSnapshot(target); err != nil {
			return cli.Envf(env, "instruction write: %v; later files were not written", err)
		}
		if err := writeFileAtomic(target.path, []byte(target.content), target.mode); err != nil {
			return cli.Envf(env, "instruction write: write target: %v", err)
		}
		actual, err := os.ReadFile(target.path)
		if err != nil || string(actual) != target.content {
			return cli.Envf(env, "instruction write: target verification failed: %s", target.path)
		}
	}
	receipt := make([]instructionWriteReceipt, 0, len(targets))
	for _, target := range targets {
		action := "written"
		if target.before != nil && target.sha256 == *target.before {
			action = "unchanged"
		}
		receipt = append(receipt, instructionWriteReceipt{Path: target.path, Action: action, SHA256: target.sha256})
	}
	if err := writeJSON(env.Stdout, receipt); err != nil {
		return cli.Envf(env, "instruction write: %v", err)
	}
	return cli.ExitOK
}

func preflightInstructionWrite(file instructionWriteFile) (instructionWriteTarget, error) {
	if file.Root == "" || file.Path == "" || len(file.Content) > maxInstructionFileBytes {
		return instructionWriteTarget{}, errors.New("root, path, and bounded content are required")
	}
	root, err := filepath.Abs(file.Root)
	if err != nil {
		return instructionWriteTarget{}, err
	}
	root = filepath.Clean(root)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return instructionWriteTarget{}, fmt.Errorf("root %s: %w", root, err)
	}
	if resolvedRoot != root {
		return instructionWriteTarget{}, fmt.Errorf("root must be canonical and must not be a symlink: %s", root)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&fs.ModeSymlink != 0 {
		return instructionWriteTarget{}, fmt.Errorf("root is not a regular directory: %s", root)
	}
	rel := filepath.FromSlash(file.Path)
	if filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return instructionWriteTarget{}, fmt.Errorf("target path must stay below its root: %q", file.Path)
	}
	path := filepath.Join(root, rel)
	if !within(root, path) {
		return instructionWriteTarget{}, fmt.Errorf("target path escapes its root: %q", file.Path)
	}
	mode := fs.FileMode(file.Mode)
	if mode.Perm() != mode || mode&0o111 != 0 || mode == 0 {
		return instructionWriteTarget{}, fmt.Errorf("target mode is not a non-executable permission mode: %o", file.Mode)
	}
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return instructionWriteTarget{}, fmt.Errorf("inspect target %s: %w", current, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return instructionWriteTarget{}, fmt.Errorf("target path contains a symlink: %s", current)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return instructionWriteTarget{}, fmt.Errorf("target parent is not a directory: %s", current)
		}
		if index == len(parts)-1 && (!info.Mode().IsRegular() || info.Size() > maxInstructionFileBytes) {
			return instructionWriteTarget{}, fmt.Errorf("target is not a bounded regular file: %s", current)
		}
	}
	target := instructionWriteTarget{path: path, content: file.Content, before: file.BeforeSHA256, mode: mode, sha256: sha256Bytes([]byte(file.Content))}
	if _, err := os.Lstat(path); err == nil {
		content, err := os.ReadFile(path)
		if err != nil {
			return instructionWriteTarget{}, fmt.Errorf("read target %s: %w", path, err)
		}
		before := sha256Bytes(content)
		if file.BeforeSHA256 == nil || *file.BeforeSHA256 != before {
			return instructionWriteTarget{}, fmt.Errorf("target changed after caller preflight: %s", path)
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		if file.BeforeSHA256 != nil {
			return instructionWriteTarget{}, fmt.Errorf("target appeared or disappeared after caller preflight: %s", path)
		}
	} else {
		return instructionWriteTarget{}, fmt.Errorf("inspect target %s: %w", path, err)
	}
	return target, nil
}

func verifyInstructionSnapshot(target instructionWriteTarget) error {
	info, err := os.Lstat(target.path)
	if target.before == nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("target appeared after preflight: %s", target.path)
	}
	if err != nil {
		return fmt.Errorf("target disappeared after preflight: %s", target.path)
	}
	if !info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("target changed type after preflight: %s", target.path)
	}
	content, err := os.ReadFile(target.path)
	if err != nil || sha256Bytes(content) != *target.before {
		return fmt.Errorf("target changed after preflight: %s", target.path)
	}
	return nil
}

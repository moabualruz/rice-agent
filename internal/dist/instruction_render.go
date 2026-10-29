package dist

import (
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moabualruz/rice-agent/internal/cli"
	"github.com/moabualruz/rice-agent/internal/paths"
)

const instructionUsage = `Usage: rice-agent instruction render --source <agent-instructions> --output <new-directory> [catalog options]
       rice-agent instruction check --catalog-export <file>

Render the source instruction layout into <output>/compiled. The output directory must not exist.
A Catalog resolve result and packet can be bundled into the output for deterministic offline use.
This command only renders derived files; it does not update instruction sources, install links, or
change harness configuration. Use --source alone as the offline source-tree fallback.

Options:
  --source <dir>            source root (for example ~/.config/agent-instructions)
  --output <dir>            new output root; compiled files are written below it
  --catalog-result <file>   complete rice-catalog resolve result JSON
  --catalog-packet <file>   matching catalog/context-packet/v1 JSON
  --catalog-export <file>   pinned local Rice Agent export from an earlier render

Exit codes: 0 rendered or valid, 1 refused, 2 usage, 3 environment.

Examples:
  rice-agent instruction render --source ~/.config/agent-instructions --output ./rendered-instructions
  rice-agent instruction render --source ~/.config/agent-instructions --catalog-result ./resolve.json --catalog-packet ./packet.json --output ./rendered-instructions
  rice-agent instruction check --catalog-export ./rendered-instructions/compiled/catalog-export.json
`

func runInstruction(env *cli.Env) int {
	if len(env.Args) == 0 || env.Args[0] == "--help" || env.Args[0] == "-h" || env.Args[0] == "help" {
		fmt.Fprint(env.Stdout, instructionUsage)
		return cli.ExitOK
	}
	if env.Args[0] == "check" {
		return runInstructionCheck(env)
	}
	if env.Args[0] != "render" {
		return cli.Usagef(env, "instruction: unknown subcommand %q; run rice-agent instruction --help", env.Args[0])
	}
	fs := flag.NewFlagSet("instruction render", flag.ContinueOnError)
	source := fs.String("source", "", "")
	output := fs.String("output", "", "")
	catalogResult := fs.String("catalog-result", "", "")
	catalogPacket := fs.String("catalog-packet", "", "")
	catalogExportPath := fs.String("catalog-export", "", "")
	sub := *env
	sub.Args = env.Args[1:]
	pos, code, ok := parseArgs(&sub, fs, instructionUsage)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return cli.Usagef(env, "instruction render: unexpected argument %q", pos[0])
	}
	if *source == "" || *output == "" {
		return cli.Usagef(env, "instruction render: --source and --output are required")
	}
	catalogPair := *catalogResult != "" || *catalogPacket != ""
	if catalogPair && (*catalogResult == "" || *catalogPacket == "") {
		return cli.Usagef(env, "instruction render: --catalog-result and --catalog-packet must be used together")
	}
	if catalogPair && *catalogExportPath != "" {
		return cli.Usagef(env, "instruction render: use either --catalog-export or the Catalog result and packet pair")
	}

	home := paths.Home(env.Getenv)
	sourcePath, err := absPath(*source, home)
	if err != nil {
		return cli.Usagef(env, "instruction render: --source: %v", err)
	}
	outputPath, err := absPath(*output, home)
	if err != nil {
		return cli.Usagef(env, "instruction render: --output: %v", err)
	}
	files, err := renderInstructionFiles(sourcePath)
	if err != nil {
		return cli.Envf(env, "instruction render: %v", err)
	}
	if catalogPair || *catalogExportPath != "" {
		export, err := readInstructionCatalogExport(home, *catalogResult, *catalogPacket, *catalogExportPath)
		if err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				return cli.Envf(env, "instruction render: %v", err)
			}
			fmt.Fprintf(env.Stderr, "rice-agent: instruction render: invalid Catalog export: %v\n", err)
			return cli.ExitNegative
		}
		bundle, err := marshalCatalogExport(export)
		if err != nil {
			fmt.Fprintf(env.Stderr, "rice-agent: instruction render: invalid Catalog export: %v\n", err)
			return cli.ExitNegative
		}
		guidance, err := renderCatalogGuidance(export)
		if err != nil {
			fmt.Fprintf(env.Stderr, "rice-agent: instruction render: invalid Catalog export: %v\n", err)
			return cli.ExitNegative
		}
		files["catalog-export.json"] = bundle
		files["CATALOG_GUIDANCE.md"] = guidance
	}
	if err := writeInstructionTree(outputPath, files); err != nil {
		if errors.Is(err, iofs.ErrExist) {
			fmt.Fprintf(env.Stderr, "rice-agent: instruction render: output directory already exists: %s\n", outputPath)
			return cli.ExitNegative
		}
		return cli.Envf(env, "instruction render: %v", err)
	}
	fmt.Fprintf(env.Stdout, "rendered %d instruction files into %s\n", len(files), filepath.Join(outputPath, "compiled"))
	return cli.ExitOK
}

func runInstructionCheck(env *cli.Env) int {
	fs := flag.NewFlagSet("instruction check", flag.ContinueOnError)
	catalogExportPath := fs.String("catalog-export", "", "")
	sub := *env
	sub.Args = env.Args[1:]
	pos, code, ok := parseArgs(&sub, fs, instructionUsage)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return cli.Usagef(env, "instruction check: unexpected argument %q", pos[0])
	}
	if *catalogExportPath == "" {
		return cli.Usagef(env, "instruction check: --catalog-export is required")
	}
	path, err := absPath(*catalogExportPath, paths.Home(env.Getenv))
	if err != nil {
		return cli.Usagef(env, "instruction check: --catalog-export: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return cli.Envf(env, "instruction check: %v", err)
	}
	export, err := catalogExportFromBytes(content)
	if err != nil {
		fmt.Fprintf(env.Stderr, "rice-agent: instruction check: invalid Catalog export: %v\n", err)
		return cli.ExitNegative
	}
	fmt.Fprintf(env.Stdout, "valid Catalog export: %s (%s)\n", export.Resolution.InputDigest, export.Resolution.PacketDigest)
	return cli.ExitOK
}

func readInstructionCatalogExport(home, resultPath, packetPath, exportPath string) (instructionCatalogExport, error) {
	if exportPath != "" {
		path, err := absPath(exportPath, home)
		if err != nil {
			return instructionCatalogExport{}, err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return instructionCatalogExport{}, err
		}
		return catalogExportFromBytes(content)
	}
	resultFile, err := absPath(resultPath, home)
	if err != nil {
		return instructionCatalogExport{}, err
	}
	packetFile, err := absPath(packetPath, home)
	if err != nil {
		return instructionCatalogExport{}, err
	}
	resultBytes, err := os.ReadFile(resultFile)
	if err != nil {
		return instructionCatalogExport{}, err
	}
	packetBytes, err := os.ReadFile(packetFile)
	if err != nil {
		return instructionCatalogExport{}, err
	}
	return catalogExportFromFiles(resultBytes, packetBytes)
}

func renderInstructionFiles(sourceRoot string) (map[string][]byte, error) {
	root, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("source root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("source root is not a directory")
	}
	read := func(name string) ([]byte, error) {
		path := filepath.Join(root, filepath.FromSlash(name))
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if !within(root, real) {
			return nil, fmt.Errorf("source %s escapes the source root", name)
		}
		info, err := os.Stat(real)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("source %s is not a regular file", name)
		}
		return os.ReadFile(real)
	}
	kernel, err := read("shared/KERNEL.md")
	if err != nil {
		return nil, err
	}
	shared, err := read("shared/AGENTS.md")
	if err != nil {
		return nil, err
	}
	codex, err := read("agents/codex.md")
	if err != nil {
		return nil, err
	}
	persona, err := read("agents/hermes-soul.md")
	if err != nil {
		return nil, err
	}
	hermesPersona, err := markedInstructionContent(persona, "agents/hermes-soul.md", "HERMES_PERSONA")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"claude", "agy", "hermes", "cursor", "opencode", "grok", "pi"} {
		if _, err := read("agents/" + name + ".md"); err != nil {
			return nil, err
		}
	}
	zvec, err := markedInstructionBlock(codex, "agents/codex.md", "ZVEC_GREP")
	if err != nil {
		return nil, err
	}
	kernelText := string(kernel)
	files := map[string][]byte{
		"AGENTS.md":                     shared,
		"KERNEL.md":                     kernel,
		"CLAUDE.md":                     []byte(importWrapper("Claude Code Global Rules", "claude")),
		"GEMINI.md":                     []byte(importWrapper("Antigravity CLI (agy) Global Rules", "agy")),
		"HERMES.md":                     []byte(importWrapper("Hermes Global Rules", "hermes")),
		"cursor-agent-instructions.mdc": []byte(cursorWrapper()),
		"codex-home-AGENTS.override.md": []byte("# Home directory\n\n<!-- agent-dry: managed sections load from shared/KERNEL.md -->\n\nShared rules are already supplied by this harness's global instruction file.\n"),
		"codex-AGENTS.md":               []byte(inlineWrapper("Codex", kernelText, "codex", zvec)),
		"grok-AGENTS.md":                []byte(inlineWrapper("Grok", kernelText, "grok", nil)),
		"pi-AGENTS.md":                  []byte(inlineWrapper("Pi", kernelText, "pi", nil)),
		"hermes-SOUL.md":                append(append(bytesTrimNewline(hermesPersona), []byte("\n\n")...), kernel...),
		"opencode-AGENTS.md":            []byte(opencodeWrapper()),
	}
	return files, nil
}

func bytesTrimNewline(b []byte) []byte {
	return []byte(strings.TrimRight(string(b), "\r\n"))
}

func importWrapper(title, agent string) string {
	return fmt.Sprintf("# %s\n\n<!-- agent-dry: managed sections load from shared/KERNEL.md -->\n\n@~/.config/agent-instructions/shared/KERNEL.md\n\n@~/.config/agent-instructions/agents/%s.md\n", title, agent)
}

func cursorWrapper() string {
	return "---\ndescription: Shared owner instructions\nalwaysApply: true\n---\n\n# Cursor Global Rules\n\n<!-- agent-dry: managed sections load from shared/KERNEL.md -->\n\n@~/.config/agent-instructions/shared/KERNEL.md\n\n@~/.config/agent-instructions/agents/cursor.md\n"
}

func opencodeWrapper() string {
	return "# OpenCode Global Rules\n\n<!-- agent-dry: managed sections load from shared/KERNEL.md -->\n\nGenerated from `~/.config/agent-instructions`. To render without overwriting this tree, run `rice-agent instruction render --source ~/.config/agent-instructions --output <new-directory>`.\n\nOpenCode loads modular instruction files through `~/.config/opencode/opencode.json` `instructions`.\n\nIf that config is unavailable, read these files before project-specific work:\n\n- `~/.config/agent-instructions/shared/KERNEL.md`\n- `~/.config/agent-instructions/agents/opencode.md`\n"
}

func inlineWrapper(harness, kernel, agent string, extra []byte) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s Global Instructions\n\nGenerated from `~/.config/agent-instructions`. To render without overwriting this tree, run `rice-agent instruction render --source ~/.config/agent-instructions --output <new-directory>`.\n\n", harness)
	out.WriteString(strings.TrimRight(kernel, "\r\n"))
	fmt.Fprintf(&out, "\n\nHarness-specific additions, read on demand:\n\n- `~/.config/agent-instructions/shared/AGENTS.md`\n- `~/.config/agent-instructions/agents/%s.md`\n", agent)
	if len(extra) > 0 {
		out.WriteString("\n")
		out.Write(extra)
		if extra[len(extra)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func markedInstructionBlock(source []byte, sourceName, name string) ([]byte, error) {
	start := []byte("<!-- " + name + "_START -->")
	end := []byte("<!-- " + name + "_END -->")
	if strings.Count(string(source), string(start)) != 1 || strings.Count(string(source), string(end)) != 1 {
		return nil, fmt.Errorf("%s must contain exactly one %s block", sourceName, name)
	}
	i := strings.Index(string(source), string(start))
	j := strings.Index(string(source), string(end))
	if j < i {
		return nil, fmt.Errorf("%s has reversed %s markers", sourceName, name)
	}
	return source[i : j+len(end)], nil
}

func markedInstructionContent(source []byte, sourceName, name string) ([]byte, error) {
	block, err := markedInstructionBlock(source, sourceName, name)
	if err != nil {
		return nil, err
	}
	start := []byte("<!-- " + name + "_START -->")
	end := []byte("<!-- " + name + "_END -->")
	content := strings.TrimSpace(string(block[len(start) : len(block)-len(end)]))
	if content == "" {
		return nil, fmt.Errorf("%s must contain non-empty %s content", sourceName, name)
	}
	return []byte(content), nil
}

func writeInstructionTree(outputRoot string, files map[string][]byte) error {
	parent := filepath.Dir(outputRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(outputRoot, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(outputRoot, ".rice-agent-render-*")
	if err != nil {
		_ = os.Remove(outputRoot)
		return err
	}
	complete := false
	defer func() {
		_ = os.RemoveAll(stage)
		if !complete {
			_ = os.Remove(outputRoot)
		}
	}()
	compiled := filepath.Join(stage, "compiled")
	if err := os.Mkdir(compiled, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if filepath.IsAbs(name) || filepath.Clean(name) != name || name == "." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid instruction output path %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(compiled, name)
		if err := writeFileAtomic(path, files[name], 0o644); err != nil {
			return err
		}
	}
	if err := os.Rename(compiled, filepath.Join(outputRoot, "compiled")); err != nil {
		return err
	}
	complete = true
	return nil
}

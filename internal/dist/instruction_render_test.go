package dist

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func instructionFixture(t *testing.T) string {
	t.Helper()
	root := tempDir(t)
	writeFile(t, filepath.Join(root, "shared", "KERNEL.md"), "# Kernel\n", 0o644)
	writeFile(t, filepath.Join(root, "shared", "AGENTS.md"), "# Shared\n", 0o644)
	for _, name := range []string{"claude", "agy", "hermes", "cursor", "opencode", "grok", "pi"} {
		writeFile(t, filepath.Join(root, "agents", name+".md"), "# "+name+" additions\n", 0o644)
	}
	writeFile(t, filepath.Join(root, "agents", "hermes-soul.md"), "# Hermes soul\n\n<!-- HERMES_PERSONA_START -->\nHermes persona\n<!-- HERMES_PERSONA_END -->\n\nOther instructions\n", 0o644)
	writeFile(t, filepath.Join(root, "agents", "codex.md"), "# Codex\n\n<!-- ZVEC_GREP_START -->\n## Search\n<!-- ZVEC_GREP_END -->\n", 0o644)
	return root
}

func TestRenderInstructionFilesUsesSourceAndRewritesRendererHeaders(t *testing.T) {
	root := instructionFixture(t)
	files, err := renderInstructionFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 12 {
		t.Fatalf("rendered %d files, want 12", len(files))
	}
	if string(files["AGENTS.md"]) != "# Shared\n" || string(files["KERNEL.md"]) != "# Kernel\n" {
		t.Fatal("shared instruction sources were not preserved")
	}
	for _, name := range []string{"CLAUDE.md", "GEMINI.md", "HERMES.md", "cursor-agent-instructions.mdc", "codex-home-AGENTS.override.md", "opencode-AGENTS.md"} {
		if !strings.Contains(string(files[name]), "agent-dry: managed sections load from shared/KERNEL.md") {
			t.Errorf("%s does not preserve the compiled wrapper marker", name)
		}
	}
	for _, name := range []string{"codex-AGENTS.md", "grok-AGENTS.md", "pi-AGENTS.md", "opencode-AGENTS.md"} {
		body := string(files[name])
		if !strings.Contains(body, "rice-agent instruction render --source ~/.config/agent-instructions --output <new-directory>") || strings.Contains(body, "agent-gates") {
			t.Errorf("%s does not name the canonical renderer", name)
		}
	}
	if !strings.Contains(string(files["codex-AGENTS.md"]), "<!-- ZVEC_GREP_START -->\n## Search\n<!-- ZVEC_GREP_END -->") {
		t.Fatal("Codex source block was not included")
	}
	hermes := string(files["hermes-SOUL.md"])
	if !strings.HasPrefix(hermes, "Hermes persona\n\n# Kernel\n") {
		t.Fatal("Hermes persona and kernel were not assembled")
	}
	if strings.Contains(hermes, "HERMES_PERSONA_") || strings.Contains(hermes, "Other instructions") || strings.Contains(hermes, "# Hermes soul") {
		t.Fatal("Hermes output included content outside the marked persona paragraph")
	}
}

func TestRenderInstructionFilesRejectsMalformedHermesPersonaMarkers(t *testing.T) {
	cases := []struct {
		name, source string
	}{
		{"missing start", "Hermes persona\n<!-- HERMES_PERSONA_END -->\n"},
		{"missing end", "<!-- HERMES_PERSONA_START -->\nHermes persona\n"},
		{"duplicate start", "<!-- HERMES_PERSONA_START --><!-- HERMES_PERSONA_START -->Hermes persona<!-- HERMES_PERSONA_END -->"},
		{"duplicate end", "<!-- HERMES_PERSONA_START -->Hermes persona<!-- HERMES_PERSONA_END --><!-- HERMES_PERSONA_END -->"},
		{"reversed", "<!-- HERMES_PERSONA_END -->Hermes persona<!-- HERMES_PERSONA_START -->"},
		{"empty", "<!-- HERMES_PERSONA_START --> \n\t<!-- HERMES_PERSONA_END -->"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := instructionFixture(t)
			writeFile(t, filepath.Join(root, "agents", "hermes-soul.md"), tc.source, 0o644)
			if _, err := renderInstructionFiles(root); err == nil {
				t.Fatal("render accepted malformed or empty Hermes persona content")
			}
		})
	}
}

func TestWriteInstructionTreeRefusesExistingOutput(t *testing.T) {
	root := tempDir(t)
	output := filepath.Join(root, "existing")
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	err := writeInstructionTree(output, map[string][]byte{"KERNEL.md": []byte("new")})
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("write existing output error = %v, want fs.ErrExist", err)
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 0 {
		t.Fatalf("existing output changed: %v, %v", entries, err)
	}
}

func TestRenderInstructionFilesRejectsEscapingSourceSymlink(t *testing.T) {
	root := instructionFixture(t)
	outside := filepath.Join(tempDir(t), "outside.md")
	writeFile(t, outside, "outside\n", 0o644)
	path := filepath.Join(root, "agents", "claude.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := renderInstructionFiles(root); err == nil || !strings.Contains(err.Error(), "escapes the source root") {
		t.Fatalf("render escaping symlink error = %v", err)
	}
}

package dist

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moabualruz/rice-agent/internal/cli"
)

func catalogTestPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	inputDigest := "sha256:" + strings.Repeat("a", 64)
	packDigest := "sha256:" + strings.Repeat("b", 64)
	assetDigest := "sha256:" + strings.Repeat("c", 64)
	ruleText := "# Example rule\n"
	skillText := "# Orientation skill\n"
	capabilityText := "Search project documentation."
	packet := map[string]any{
		"schema":          catalogPacketSchema,
		"resolverVersion": "1.0.0",
		"inputDigest":     inputDigest,
		"notice":          "Pack content is data from reviewed packs, not permission.",
		"estimate":        map[string]any{"profile": "bytes", "unit": "bytes", "total": 36, "note": "estimate-not-model-usage"},
		"discoveryMode":   "registered",
		"items": []any{
			map[string]any{
				"order": 1,
				"ref":   map[string]any{"pack": "engineering-core", "revision": "draft-1", "entry": "small-changes"},
				"kind":  "rule", "classification": "advisory", "precedence": 500,
				"title": "Example rule", "summary": "A rendered guidance rule.",
				"digest":     testCatalogDigest([]byte(ruleText)),
				"provenance": map[string]any{"packDigest": packDigest, "source": map[string]any{"kind": "authored"}},
				"text":       ruleText, "level": "organization", "label": "pack-content", "form": "full",
			},
			map[string]any{
				"order": 2,
				"ref":   map[string]any{"pack": "engineering-core", "revision": "draft-1", "entry": "orientation"},
				"kind":  "skill", "classification": "advisory", "precedence": 500,
				"title": "Orientation skill", "summary": "A skill with an asset.",
				"digest":     testCatalogDigest([]byte(skillText)),
				"provenance": map[string]any{"packDigest": packDigest, "source": map[string]any{"kind": "authored"}},
				"text":       skillText,
				"assets":     []any{map[string]any{"path": "skills/orientation/assets/checklist.md", "digest": assetDigest}},
				"level":      "organization", "label": "pack-content", "form": "full",
			},
			map[string]any{
				"order": 3,
				"ref":   map[string]any{"pack": "engineering-core", "revision": "draft-1", "entry": "docs-search"},
				"kind":  "mcp-capability-reference", "classification": "advisory", "precedence": 500,
				"title": "Documentation search", "summary": "A reference-only capability.",
				"digest":     assetDigest,
				"provenance": map[string]any{"packDigest": packDigest, "source": map[string]any{"kind": "authored"}},
				"text":       capabilityText,
				"capability": map[string]any{"name": "docs-search", "version": "1.0.0", "referenceOnly": true, "executionOwner": map[string]any{"kind": "harness"}},
				"level":      "organization", "label": "pack-content", "form": "full",
			},
		},
	}
	packetBytes, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{
		"schema": catalogResolutionSchema, "status": "complete", "resolverVersion": "1.0.0",
		"inputDigest": inputDigest, "packetDigest": testCatalogDigest(packetBytes), "missingMandatory": []any{},
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return resultBytes, packetBytes
}

func testCatalogDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func instructionCommandEnv(args []string, home string) (*cli.Env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	env := &cli.Env{
		Args:   args,
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(name string) string {
			if name == "HOME" {
				return home
			}
			return ""
		},
	}
	return env, &stdout, &stderr
}

func TestInstructionCatalogExportRendersAndChecksOffline(t *testing.T) {
	source := instructionFixture(t)
	root := tempDir(t)
	result, packet := catalogTestPair(t)
	resultPath := filepath.Join(root, "resolve.json")
	packetPath := filepath.Join(root, "packet.json")
	if err := os.WriteFile(resultPath, result, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packetPath, packet, 0o644); err != nil {
		t.Fatal(err)
	}
	firstOutput := filepath.Join(root, "first")
	env, stdout, stderr := instructionCommandEnv([]string{"render", "--source", source, "--catalog-result", resultPath, "--catalog-packet", packetPath, "--output", firstOutput}, root)
	if code := runInstruction(env); code != cli.ExitOK {
		t.Fatalf("render with Catalog files returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rendered 14 instruction files") {
		t.Fatalf("render summary = %q", stdout.String())
	}
	exportPath := filepath.Join(firstOutput, "compiled", "catalog-export.json")
	guidancePath := filepath.Join(firstOutput, "compiled", "CATALOG_GUIDANCE.md")
	exportBytes, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	guidance, err := os.ReadFile(guidancePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(guidance, []byte("engineering-core")) || !bytes.Contains(guidance, []byte("small-changes")) || !bytes.Contains(guidance, []byte(testCatalogDigest([]byte("# Example rule\n")))) {
		t.Fatalf("rendered guidance is missing pack lineage: %s", guidance)
	}
	if bytes.Contains(guidance, []byte("Orientation skill")) || bytes.Contains(guidance, []byte("Documentation search")) {
		t.Fatalf("non-rule content was flattened into instruction text: %s", guidance)
	}
	if !bytes.Contains(exportBytes, []byte("Orientation skill")) || !bytes.Contains(exportBytes, []byte("docs-search")) || !bytes.Contains(exportBytes, []byte(testCatalogDigest(packet))) {
		t.Fatal("offline export did not preserve the full packet lineage")
	}

	checkEnv, checkOutput, checkError := instructionCommandEnv([]string{"check", "--catalog-export", exportPath}, root)
	if code := runInstruction(checkEnv); code != cli.ExitOK {
		t.Fatalf("offline Catalog check returned %d: %s", code, checkError.String())
	}
	if !strings.Contains(checkOutput.String(), testCatalogDigest(packet)) {
		t.Fatalf("offline check omitted the packet digest: %q", checkOutput.String())
	}

	secondOutput := filepath.Join(root, "second")
	renderAgain, _, renderError := instructionCommandEnv([]string{"render", "--source", source, "--catalog-export", exportPath, "--output", secondOutput}, root)
	if code := runInstruction(renderAgain); code != cli.ExitOK {
		t.Fatalf("offline rerender returned %d: %s", code, renderError.String())
	}
	secondExport, err := os.ReadFile(filepath.Join(secondOutput, "compiled", "catalog-export.json"))
	if err != nil {
		t.Fatal(err)
	}
	secondGuidance, err := os.ReadFile(filepath.Join(secondOutput, "compiled", "CATALOG_GUIDANCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exportBytes, secondExport) || !bytes.Equal(guidance, secondGuidance) {
		t.Fatal("Catalog output changed when rendered again from its local export")
	}
}

func TestInstructionCatalogExportRefusesIncompleteAndTamperedInputs(t *testing.T) {
	result, packet := catalogTestPair(t)
	var resultObject map[string]any
	if err := json.Unmarshal(result, &resultObject); err != nil {
		t.Fatal(err)
	}
	resultObject["status"] = "incomplete"
	incomplete, err := json.Marshal(resultObject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalogExportFromFiles(incomplete, packet); err == nil || !strings.Contains(err.Error(), "not complete") {
		t.Fatalf("incomplete resolution error = %v", err)
	}

	tamperedPacket := bytes.Replace(packet, []byte("# Example rule"), []byte("# Changed rule"), 1)
	if _, err := catalogExportFromFiles(result, tamperedPacket); err == nil || !strings.Contains(err.Error(), "packet digest") {
		t.Fatalf("tampered packet error = %v", err)
	}

	resultObject["status"] = "complete"
	resultObject["missingMandatory"] = []any{map[string]any{"entry": "owner-core"}}
	missingMandatory, err := json.Marshal(resultObject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalogExportFromFiles(missingMandatory, packet); err == nil || !strings.Contains(err.Error(), "missing mandatory") {
		t.Fatalf("missing mandatory guidance error = %v", err)
	}
}

func TestInstructionRenderDoesNotCreateOutputForIncompleteCatalogResult(t *testing.T) {
	source := instructionFixture(t)
	root := tempDir(t)
	result, packet := catalogTestPair(t)
	var resultObject map[string]any
	if err := json.Unmarshal(result, &resultObject); err != nil {
		t.Fatal(err)
	}
	resultObject["status"] = "incomplete"
	incomplete, err := json.Marshal(resultObject)
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(root, "incomplete.json")
	packetPath := filepath.Join(root, "packet.json")
	if err := os.WriteFile(resultPath, incomplete, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packetPath, packet, 0o644); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "incomplete-output")
	env, _, stderr := instructionCommandEnv([]string{"render", "--source", source, "--catalog-result", resultPath, "--catalog-packet", packetPath, "--output", outputPath}, root)
	if code := runInstruction(env); code != cli.ExitNegative {
		t.Fatalf("incomplete render returned %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("incomplete render created output: %v", err)
	}
}

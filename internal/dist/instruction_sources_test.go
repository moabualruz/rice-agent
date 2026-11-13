package dist

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/moabualruz/rice-agent/internal/cli"
)

func instructionSourceFixture(t *testing.T) (string, string, string, string, map[string]any) {
	t.Helper()
	root := tempDir(t)
	target := filepath.Join(root, "shared", "modules", "small-changes.md")
	writeFile(t, target, "# Legacy authored copy\n", 0o640)
	result, packet := catalogTestPair(t)
	var resolution any
	if err := json.Unmarshal(result, &resolution); err != nil {
		t.Fatal(err)
	}
	export, err := json.Marshal(map[string]any{"schema": instructionCatalogExportSchema, "resolution": resolution, "packet": json.RawMessage(packet)})
	if err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(root, "export.json")
	writeFile(t, exportPath, string(export), 0o644)
	mapping := map[string]any{
		"schema": "rice-agent/instruction-sources/v1", "root": root,
		"files": []any{map[string]any{
			"path": "shared/modules/small-changes.md", "before_sha256": sha256Bytes([]byte("# Legacy authored copy\n")), "mode": 0o640,
			"ref":    map[string]any{"pack": "engineering-core", "revision": "draft-1", "entry": "small-changes"},
			"digest": testCatalogDigest([]byte("# Example rule\n")),
		}},
	}
	mappingBytes, err := json.Marshal(mapping)
	if err != nil {
		t.Fatal(err)
	}
	mappingPath := filepath.Join(root, "mapping.json")
	writeFile(t, mappingPath, string(mappingBytes), 0o644)
	return root, target, exportPath, mappingPath, mapping
}

func TestInstructionSourcePlanMaterializesPinnedRuleThroughWriter(t *testing.T) {
	root, target, exportPath, mappingPath, _ := instructionSourceFixture(t)
	env, plan, stderr := instructionCommandEnv([]string{"source-plan", "--catalog-export", exportPath, "--mapping", mappingPath}, root)
	if code := runInstruction(env); code != cli.ExitOK {
		t.Fatalf("source-plan returned %d: %s", code, stderr.String())
	}
	before, err := os.ReadFile(target)
	if err != nil || string(before) != "# Legacy authored copy\n" {
		t.Fatalf("planning changed target: %q, %v", before, err)
	}
	writer, _, writerError := instructionCommandEnv([]string{"write", "--plan", "-"}, root)
	writer.Stdin = bytes.NewReader(plan.Bytes())
	if code := runInstruction(writer); code != cli.ExitOK {
		t.Fatalf("write returned %d: %s", code, writerError.String())
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != "# Example rule\n" {
		t.Fatalf("migrated rule = %q, %v; want exact authored Catalog bytes", actual, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("migrated rule mode = %v, %v", info, err)
	}
	var decoded struct {
		Files []struct {
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := json.Unmarshal(plan.Bytes(), &decoded); err != nil || len(decoded.Files) != 1 {
		t.Fatalf("plan is not the existing single-target writer contract: %v", err)
	}
	repeat, repeatedPlan, repeatedError := instructionCommandEnv([]string{"source-plan", "--catalog-export", exportPath, "--mapping", mappingPath}, root)
	if code := runInstruction(repeat); code != cli.ExitOK {
		t.Fatalf("repeated plan returned %d: %s", code, repeatedError.String())
	}
	repeatedWriter, receipt, repeatedWriterError := instructionCommandEnv([]string{"write", "--plan", "-"}, root)
	repeatedWriter.Stdin = bytes.NewReader(repeatedPlan.Bytes())
	if code := runInstruction(repeatedWriter); code != cli.ExitOK || !bytes.Contains(receipt.Bytes(), []byte(`"unchanged"`)) {
		t.Fatalf("repeated write returned %d: %s, receipt %s", code, repeatedWriterError.String(), receipt.String())
	}
}

func TestInstructionSourcePlanRejectsInvalidMappingsWithoutOutput(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, root, target string, mapping map[string]any)
	}{
		{"missing rule", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["files"].([]any)[0].(map[string]any)["ref"].(map[string]any)["entry"] = "missing"
		}},
		{"wrong revision", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["files"].([]any)[0].(map[string]any)["ref"].(map[string]any)["revision"] = "other"
		}},
		{"wrong digest", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["files"].([]any)[0].(map[string]any)["digest"] = testCatalogDigest([]byte("other"))
		}},
		{"non-rule entry", func(t *testing.T, root, target string, mapping map[string]any) {
			file := mapping["files"].([]any)[0].(map[string]any)
			file["ref"].(map[string]any)["entry"] = "orientation"
			file["digest"] = testCatalogDigest([]byte("# Orientation skill\n"))
		}},
		{"duplicate ownership", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["files"] = append(mapping["files"].([]any), mapping["files"].([]any)[0])
		}},
		{"path escape", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["files"].([]any)[0].(map[string]any)["path"] = "../escape.md"
		}},
		{"owner edit", func(t *testing.T, root, target string, mapping map[string]any) {
			writeFile(t, target, "owner changed this\n", 0o640)
		}},
		{"missing target", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["files"].([]any)[0].(map[string]any)["path"] = "missing.md"
		}},
		{"symlink target", func(t *testing.T, root, target string, mapping map[string]any) {
			link := filepath.Join(root, "link.md")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			mapping["files"].([]any)[0].(map[string]any)["path"] = "link.md"
		}},
		{"unknown field", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["permissions"] = "allow"
		}},
		{"bad schema", func(t *testing.T, root, target string, mapping map[string]any) {
			mapping["schema"] = "other"
		}},
		{"relative root", func(t *testing.T, root, target string, mapping map[string]any) {
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			relative, err := filepath.Rel(cwd, root)
			if err != nil {
				t.Fatal(err)
			}
			mapping["root"] = relative
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, target, exportPath, mappingPath, mapping := instructionSourceFixture(t)
			tc.change(t, root, target, mapping)
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(mapping)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, mappingPath, string(data), 0o644)
			env, output, stderr := instructionCommandEnv([]string{"source-plan", "--catalog-export", exportPath, "--mapping", mappingPath}, root)
			if code := runInstruction(env); code == cli.ExitOK || output.Len() != 0 {
				t.Fatalf("invalid mapping produced plan: code %d, stdout %s, stderr %s", code, output.String(), stderr.String())
			}
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("planning changed target: %v", err)
			}
		})
	}
}

func TestInstructionSourcePlanRejectsEmptyRule(t *testing.T) {
	root, target, exportPath, mappingPath, mapping := instructionSourceFixture(t)
	data, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	var export map[string]any
	if err := json.Unmarshal(data, &export); err != nil {
		t.Fatal(err)
	}
	packet := export["packet"].(map[string]any)
	item := packet["items"].([]any)[0].(map[string]any)
	item["text"] = ""
	item["digest"] = testCatalogDigest(nil)
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	export["packet"] = json.RawMessage(packetData)
	export["resolution"].(map[string]any)["packetDigest"] = testCatalogDigest(packetData)
	data, err = json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, exportPath, string(data), 0o644)
	mapping["files"].([]any)[0].(map[string]any)["digest"] = testCatalogDigest(nil)
	data, err = json.Marshal(mapping)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, mappingPath, string(data), 0o644)
	env, output, stderr := instructionCommandEnv([]string{"source-plan", "--catalog-export", exportPath, "--mapping", mappingPath}, root)
	if code := runInstruction(env); code == cli.ExitOK || output.Len() != 0 {
		t.Fatalf("empty rule produced plan: code %d, stdout %s, stderr %s", code, output.String(), stderr.String())
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != "# Legacy authored copy\n" {
		t.Fatalf("empty rule changed target: %q, %v", actual, err)
	}
}

func TestInstructionSourcePlanBindsLaterWriterToSnapshot(t *testing.T) {
	root, target, exportPath, mappingPath, _ := instructionSourceFixture(t)
	env, plan, stderr := instructionCommandEnv([]string{"source-plan", "--catalog-export", exportPath, "--mapping", mappingPath}, root)
	if code := runInstruction(env); code != cli.ExitOK {
		t.Fatalf("source-plan returned %d: %s", code, stderr.String())
	}
	writeFile(t, target, "later owner edit\n", 0o640)
	writer, _, writerError := instructionCommandEnv([]string{"write", "--plan", "-"}, root)
	writer.Stdin = bytes.NewReader(plan.Bytes())
	if code := runInstruction(writer); code == cli.ExitOK {
		t.Fatalf("write accepted later edit: %s", writerError.String())
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != "later owner edit\n" {
		t.Fatalf("writer replaced later edit: %q, %v", actual, err)
	}
}

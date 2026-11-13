package dist

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/moabualruz/rice-agent/internal/cli"
	"github.com/moabualruz/rice-agent/internal/paths"
)

const instructionSourceUsage = `Usage: rice-agent instruction source-plan --catalog-export <file> --mapping <file>

Plan migrated rule files from a verified local Catalog export. The mapping explicitly names a
canonical root, target paths, expected previous hashes, permission modes, exact entry references,
and entry digests. Only rule text is materialized, byte for byte from the pinned export.

Mapping schema: rice-agent/instruction-sources/v1
Output schema: rice-agent/instruction-write/v1, usable with instruction write --plan -.
Planning preflights every target but writes nothing. Retain the mapping and Catalog export with
the plan for pack/entry lineage. An already matching rule is unchanged on repeated plans.
No Catalog, vault, model, network access, or harness configuration change is required.

Exit codes: 0 planned, 1 refused, 2 usage, 3 environment.
`

type instructionSourceMapping struct {
	Schema string                  `json:"schema"`
	Root   string                  `json:"root"`
	Files  []instructionSourceFile `json:"files"`
}

type instructionSourceFile struct {
	Path         string          `json:"path"`
	BeforeSHA256 *string         `json:"before_sha256"`
	Mode         uint32          `json:"mode"`
	Ref          catalogEntryRef `json:"ref"`
	Digest       string          `json:"digest"`
}

func runInstructionSourcePlan(env *cli.Env) int {
	fs := flag.NewFlagSet("instruction source-plan", flag.ContinueOnError)
	exportPath := fs.String("catalog-export", "", "")
	mappingPath := fs.String("mapping", "", "")
	sub := *env
	sub.Args = env.Args[1:]
	pos, code, ok := parseArgs(&sub, fs, instructionSourceUsage)
	if !ok {
		return code
	}
	if len(pos) != 0 || *exportPath == "" || *mappingPath == "" {
		return cli.Usagef(env, "instruction source-plan: --catalog-export and --mapping are required, without positional arguments")
	}
	home := paths.Home(env.Getenv)
	export, err := readInstructionCatalogExport(home, "", "", *exportPath)
	if err != nil {
		fmt.Fprintf(env.Stderr, "rice-agent: instruction source-plan: invalid Catalog export: %v\n", err)
		return cli.ExitNegative
	}
	mappingFile, err := absPath(*mappingPath, home)
	if err != nil {
		return cli.Usagef(env, "instruction source-plan: --mapping: %v", err)
	}
	file, err := os.Open(mappingFile)
	if err != nil {
		return cli.Envf(env, "instruction source-plan: %v", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxInstructionPlanBytes+1))
	if err != nil {
		return cli.Envf(env, "instruction source-plan: read mapping: %v", err)
	}
	if len(data) > maxInstructionPlanBytes {
		return cli.Usagef(env, "instruction source-plan: mapping exceeds size limit")
	}
	var mapping instructionSourceMapping
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mapping); err != nil {
		return cli.Usagef(env, "instruction source-plan: invalid mapping: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return cli.Usagef(env, "instruction source-plan: mapping must contain exactly one JSON value")
	}
	if mapping.Schema != "rice-agent/instruction-sources/v1" || mapping.Root == "" || len(mapping.Files) == 0 || len(mapping.Files) > maxInstructionPlanFiles {
		return cli.Usagef(env, "instruction source-plan: invalid mapping schema, root, or file count")
	}
	if !filepath.IsAbs(mapping.Root) || filepath.Clean(mapping.Root) != mapping.Root {
		return cli.Usagef(env, "instruction source-plan: mapping root must be an absolute canonical path")
	}
	plan, err := planInstructionSources(export, mapping)
	if err != nil {
		fmt.Fprintf(env.Stderr, "rice-agent: instruction source-plan: %v; nothing changed\n", err)
		return cli.ExitNegative
	}
	if err := writeJSON(env.Stdout, plan); err != nil {
		return cli.Envf(env, "instruction source-plan: %v", err)
	}
	return cli.ExitOK
}

func planInstructionSources(export instructionCatalogExport, mapping instructionSourceMapping) (instructionWritePlan, error) {
	var packet catalogPacket
	if err := json.Unmarshal(export.Packet, &packet); err != nil {
		return instructionWritePlan{}, err
	}
	entries := make(map[catalogEntryRef]catalogPacketItem, len(packet.Items))
	for _, item := range packet.Items {
		if _, exists := entries[item.Ref]; exists {
			return instructionWritePlan{}, errors.New("Catalog packet has ambiguous entry ownership")
		}
		entries[item.Ref] = item
	}
	plan := instructionWritePlan{Schema: "rice-agent/instruction-write/v1"}
	seenPaths := make(map[string]bool, len(mapping.Files))
	seenEntries := make(map[catalogEntryRef]bool, len(mapping.Files))
	for _, source := range mapping.Files {
		item, exists := entries[source.Ref]
		if !exists || item.Kind != "rule" || item.Text == nil || strings.TrimSpace(*item.Text) == "" || item.Digest != source.Digest {
			return instructionWritePlan{}, fmt.Errorf("mapped rule %s/%s at revision %s is missing or its digest does not match", source.Ref.Pack, source.Ref.Entry, source.Ref.Revision)
		}
		if seenEntries[source.Ref] {
			return instructionWritePlan{}, fmt.Errorf("mapped rule %s/%s has multiple targets", source.Ref.Pack, source.Ref.Entry)
		}
		seenEntries[source.Ref] = true
		file := instructionWriteFile{Root: mapping.Root, Path: source.Path, BeforeSHA256: source.BeforeSHA256, Content: *item.Text, Mode: source.Mode}
		target, err := preflightInstructionWrite(file)
		if err != nil {
			// A previously materialized rule is the only permitted alternative to the declared old snapshot.
			matching := sha256Bytes([]byte(file.Content))
			file.BeforeSHA256 = &matching
			target, err = preflightInstructionWrite(file)
			if err != nil {
				return instructionWritePlan{}, err
			}
		}
		if seenPaths[target.path] {
			return instructionWritePlan{}, fmt.Errorf("duplicate mapped target %s", target.path)
		}
		seenPaths[target.path] = true
		plan.Files = append(plan.Files, file)
	}
	data, err := marshalJSON(plan)
	if err != nil || len(data) > maxInstructionPlanBytes {
		return instructionWritePlan{}, errors.New("generated write plan exceeds size limit")
	}
	return plan, nil
}

// Package manifest loads and validates tool/v1 manifests against the embedded JSON Schema.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/moabualruz/rice-agent/schemas"
)

// FileName is the manifest file inside each tool directory.
const FileName = "tool.json"

// Tool is a tool/v1 manifest. Field meanings are in the rice-agent spec, section 6.2.
type Tool struct {
	Schema       string            `json:"schema"`
	Name         string            `json:"name"`
	Summary      string            `json:"summary"`
	Intents      []string          `json:"intents"`
	Tags         []string          `json:"tags,omitempty"`
	Kind         string            `json:"kind"`
	Scope        string            `json:"scope"`
	Entry        string            `json:"entry"`
	Language     string            `json:"language"`
	SideEffects  string            `json:"side_effects"`
	DryRun       string            `json:"dry_run,omitempty"`
	ExitCodes    map[string]string `json:"exit_codes"`
	Platforms    []string          `json:"platforms"`
	Requires     []string          `json:"requires,omitempty"`
	Config       []string          `json:"config,omitempty"`
	Test         string            `json:"test"`
	Replaces     []string          `json:"replaces,omitempty"`
	Status       string            `json:"status"`
	Supersedes   []string          `json:"supersedes,omitempty"`
	SupersededBy string            `json:"superseded_by,omitempty"`
	Extends      string            `json:"extends,omitempty"`
	DistinctFrom []Distinct        `json:"distinct_from,omitempty"`
	Origin       Origin            `json:"origin"`

	// Dir is the absolute tool directory; not part of the manifest.
	Dir string `json:"-"`
}

// Distinct records why a tool is kept apart from a similar one.
type Distinct struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

// Origin is the provenance of a tool.
type Origin struct {
	Date         string `json:"date"`
	Builder      string `json:"builder"`
	Task         string `json:"task"`
	PromotedFrom string `json:"promoted_from,omitempty"`
}

// EntryPath is the absolute path of the tool's executable.
func (t *Tool) EntryPath() string { return filepath.Join(t.Dir, t.Entry) }

// Listed reports whether lookups should return the tool.
func (t *Tool) Listed() bool { return t.Status == "trial" || t.Status == "active" }

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
)

func schema() (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.ToolV1))
		if err != nil {
			compileErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(schemas.ToolV1ID, doc); err != nil {
			compileErr = err
			return
		}
		compiled, compileErr = c.Compile(schemas.ToolV1ID)
	})
	return compiled, compileErr
}

// Validate checks raw manifest bytes against tool/v1.
func Validate(raw []byte) error {
	s, err := schema()
	if err != nil {
		return fmt.Errorf("compile tool/v1 schema: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	return s.Validate(inst)
}

// Load reads, validates and decodes the manifest in dir.
func Load(dir string) (*Tool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(abs, FileName))
	if err != nil {
		return nil, err
	}
	if err := Validate(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(abs, FileName), err)
	}
	var t Tool
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(abs, FileName), err)
	}
	if filepath.Base(abs) != t.Name {
		return nil, fmt.Errorf("%s: name %q does not match directory %q", filepath.Join(abs, FileName), t.Name, filepath.Base(abs))
	}
	t.Dir = abs
	return &t, nil
}

// LoadAll loads every tools/<name>/tool.json under root, sorted by name. It returns the
// tools that loaded and a joined error naming each manifest that did not.
func LoadAll(toolsRoot string) ([]*Tool, error) {
	entries, err := os.ReadDir(toolsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var tools []*Tool
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(toolsRoot, e.Name())
		if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
			continue
		}
		t, err := Load(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, errors.Join(errs...)
}

package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/moabualruz/agent-dry/schemas"
)

// ProviderSchema is the schema value registry/providers.json must carry.
const ProviderSchema = "provider/v1"

// ProvidersFile is the provider registry, relative to the agent-dry root.
const ProvidersFile = "registry/providers.json"

// Provider is one provider/v1 entry: a mature CLI installed from upstream or from another owner
// project. agent-dry lists it as data so find and show surface it; it ships no wrapper code.
type Provider struct {
	Name                string   `json:"name"`
	Summary             string   `json:"summary"`
	Intents             []string `json:"intents"`
	Command             string   `json:"command"` // executable name looked up on PATH
	Owner               string   `json:"owner"`   // upstream:<url> or project:<owner>/<repo>
	License             string   `json:"license"` // SPDX expression
	Install             string   `json:"install"` // one-line install hint
	Homepage            string   `json:"homepage"`
	Notes               string   `json:"notes,omitempty"`
	RequiresPolicyCheck bool     `json:"requires_policy_check,omitempty"` // Python inside: use only its supported CLI
}

var (
	providerOnce      sync.Once
	providerFileSch   *jsonschema.Schema
	providerEntrySch  *jsonschema.Schema
	providerSchemaErr error
)

// providerSchemas compiles the embedded provider/v1 schema for the whole file and for one entry.
func providerSchemas() (file, entry *jsonschema.Schema, err error) {
	providerOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.ProviderV1))
		if err != nil {
			providerSchemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(schemas.ProviderV1ID, doc); err != nil {
			providerSchemaErr = err
			return
		}
		if providerFileSch, err = c.Compile(schemas.ProviderV1ID); err != nil {
			providerSchemaErr = err
			return
		}
		providerEntrySch, providerSchemaErr = c.Compile(schemas.ProviderV1ID + "#/$defs/provider")
	})
	return providerFileSch, providerEntrySch, providerSchemaErr
}

// ValidateProviders checks raw registry/providers.json bytes against provider/v1 and rejects a
// name used twice.
func ValidateProviders(raw []byte) error {
	file, _, err := providerSchemas()
	if err != nil {
		return fmt.Errorf("compile provider/v1 schema: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("parse providers: %w", err)
	}
	if err := file.Validate(inst); err != nil {
		return errors.New(oneLine(err))
	}
	var pf struct {
		Providers []Provider `json:"providers"`
	}
	if err := json.Unmarshal(raw, &pf); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range pf.Providers {
		if seen[p.Name] {
			return fmt.Errorf("provider %q listed twice", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// oneLine flattens a multi-line schema error into one warning line.
func oneLine(err error) string {
	var parts []string
	for _, l := range strings.Split(err.Error(), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- "))
		if l == "" || strings.HasPrefix(l, "jsonschema validation failed") {
			continue
		}
		parts = append(parts, l)
	}
	if len(parts) == 0 {
		return strings.TrimSpace(err.Error())
	}
	return strings.Join(parts, "; ")
}

// readProviders reads registry/providers.json. A missing file means no providers. A file that
// does not parse or carries another schema value is skipped with a warning, and so is each entry
// that fails provider/v1 or repeats a name; the other entries still load.
func readProviders(path string, st *[]stamp) ([]*doc, []string) {
	*st = append(*st, stampOf(path))
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("skipped providers: %v", err)}
	}
	_, entry, err := providerSchemas()
	if err != nil {
		return nil, []string{fmt.Sprintf("skipped providers: compile provider/v1 schema: %v", err)}
	}
	var pf struct {
		Schema    string            `json:"schema"`
		Providers []json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, []string{fmt.Sprintf("skipped providers %s: %v", path, err)}
	}
	if pf.Schema != ProviderSchema {
		return nil, []string{fmt.Sprintf("skipped providers %s: schema %q, want %q", path, pf.Schema, ProviderSchema)}
	}
	var warns []string
	var ps []Provider
	seen := map[string]bool{}
	for i, rm := range pf.Providers {
		label := fmt.Sprintf("#%d", i+1)
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(rm, &named) == nil && named.Name != "" {
			label = named.Name
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(rm))
		if err == nil {
			err = entry.Validate(inst)
		}
		var p Provider
		if err == nil {
			dec := json.NewDecoder(bytes.NewReader(rm))
			dec.DisallowUnknownFields()
			err = dec.Decode(&p)
		}
		if err != nil {
			warns = append(warns, fmt.Sprintf("skipped provider %s: %s", label, oneLine(err)))
			continue
		}
		if seen[p.Name] {
			warns = append(warns, fmt.Sprintf("skipped provider %s: listed twice", p.Name))
			continue
		}
		seen[p.Name] = true
		ps = append(ps, p)
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	docs := make([]*doc, 0, len(ps))
	for i := range ps {
		p := ps[i]
		var tags []string
		if !strings.EqualFold(p.Command, p.Name) {
			tags = []string{p.Command}
		}
		docs = append(docs, &doc{
			Name:     p.Name,
			Summary:  p.Summary,
			Intents:  p.Intents,
			Tags:     tags,
			Kind:     kindExternal,
			Scope:    "global",
			Entry:    p.Command,
			Source:   srcExternal,
			Path:     path,
			Provider: &p,
		})
	}
	return docs, warns
}

// installedAt is where the provider's command resolves on the PATH getenv reports, or "".
func (ix *Index) installedAt(p *Provider) string { return lookPath(ix.getenv, p.Command) }

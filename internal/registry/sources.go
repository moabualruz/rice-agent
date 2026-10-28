package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/moabualruz/rice-agent/internal/manifest"
)

// WantedSchema is the schema value registry/wanted.json must carry.
const WantedSchema = "wanted/v1"

// cacheVersion changes whenever the cached document shape changes.
const cacheVersion = 2

// IndexFile is the cache file name inside StateDir.
const IndexFile = "index.json"

// stamp records one source path's modification time and size; M is -1 when the path is missing.
type stamp struct {
	P string `json:"p"`
	M int64  `json:"m"`
	S int64  `json:"s"`
}

func stampOf(p string) stamp {
	info, err := os.Stat(p)
	if err != nil {
		return stamp{P: p, M: -1}
	}
	return stamp{P: p, M: info.ModTime().UnixNano(), S: info.Size()}
}

// cacheFile is StateDir/index.json: the cached sources and the stamps that invalidate them.
type cacheFile struct {
	Version   int      `json:"version"`
	Root      string   `json:"root"`
	Home      string   `json:"home"`
	Stamps    []stamp  `json:"stamps"`
	Tools     []*doc   `json:"tools"`
	Skills    []*doc   `json:"skills"`
	Installed []*doc   `json:"installed"`
	Providers []*doc   `json:"providers"`
	Wanted    []*doc   `json:"wanted"`
	Warnings  []string `json:"warnings,omitempty"`
}

func (c *cacheFile) fresh() bool {
	for _, s := range c.Stamps {
		if stampOf(s.P) != s {
			return false
		}
	}
	return true
}

func (ix *Index) indexPath() string { return filepath.Join(ix.stateDir, IndexFile) }

// cached returns the cached sources, rebuilding and rewriting the cache when it is missing, stale,
// corrupt or rebuild is set. Only an index that exists but cannot be read is an error.
func (ix *Index) cached(rebuild bool) (*cacheFile, error) {
	path := ix.indexPath()
	if !rebuild {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			var c cacheFile
			if json.Unmarshal(raw, &c) == nil && c.Version == cacheVersion && c.Root == ix.root && c.Home == ix.home && c.fresh() {
				for _, d := range c.Tools {
					if d.Tool != nil {
						d.Tool.Dir = d.Path
					}
				}
				ix.fromCache = true
				return &c, nil
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, fmt.Errorf("unreadable index %s: %w", path, err)
		}
	}
	c, err := buildCache(ix.root, ix.home)
	if err != nil {
		return nil, err
	}
	ix.cacheErr = writeCache(ix.stateDir, path, c)
	ix.syncCorpus(c)
	return c, nil
}

// writeCache replaces the index atomically (temporary file in the same directory, then rename).
func writeCache(dir, path string, c *cacheFile) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".index-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// buildCache reads the rice-agent tools, rice-agent skills, installed skills under home, the
// external providers and the wanted families. Paths are stamped before they are read so a
// concurrent edit invalidates; a missing providers.json is stamped too, so creating it does.
func buildCache(root, home string) (*cacheFile, error) {
	c := &cacheFile{Version: cacheVersion, Root: root, Home: home}
	st := &c.Stamps

	tools, warns, err := readTools(filepath.Join(root, "tools"), st)
	if err != nil {
		return nil, err
	}
	c.Tools, c.Warnings = tools, warns
	c.Skills = readSkills(filepath.Join(root, "skills"), st, srcAgentDry, "global")
	c.Installed = append(readSkills(filepath.Join(home, ".agents", "skills"), st, srcSkill, "global"),
		readSkills(filepath.Join(home, ".claude", "skills"), st, srcSkill, "global")...)
	providers, pwarns := readProviders(filepath.Join(root, ProvidersFile), st)
	c.Providers = providers
	c.Warnings = append(c.Warnings, pwarns...)
	c.Wanted, err = readWanted(filepath.Join(root, "registry", "wanted.json"), st)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func readTools(dir string, st *[]stamp) ([]*doc, []string, error) {
	*st = append(*st, stampOf(dir))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("unreadable tools directory: %w", err)
	}
	for _, e := range entries {
		sub := filepath.Join(dir, e.Name())
		*st = append(*st, stampOf(sub), stampOf(filepath.Join(sub, manifest.FileName)))
	}
	tools, err := manifest.LoadAll(dir)
	var warns []string
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				warns = append(warns, "skipped manifest: "+line)
			}
		}
	}
	docs := make([]*doc, 0, len(tools))
	for _, t := range tools {
		docs = append(docs, &doc{
			Name:    t.Name,
			Summary: t.Summary,
			Intents: t.Intents,
			Tags:    t.Tags,
			Kind:    t.Kind,
			Scope:   t.Scope,
			Entry:   t.EntryPath(),
			Source:  srcAgentDry,
			Path:    t.Dir,
			Hidden:  !t.Listed(),
			Tool:    t,
		})
	}
	return docs, warns, nil
}

// readSkills reads <dir>/*/SKILL.md frontmatter, following symlinks. st may be nil (live sources).
func readSkills(dir string, st *[]stamp, source, scope string) []*doc {
	if st != nil {
		*st = append(*st, stampOf(dir))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var docs []*doc
	seen := map[string]bool{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name(), "SKILL.md")
		if st != nil {
			*st = append(*st, stampOf(filepath.Join(dir, e.Name())), stampOf(p))
		}
		name, desc, ok := frontmatter(p)
		if !ok {
			continue
		}
		if name == "" {
			name = e.Name()
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		docs = append(docs, &doc{Name: name, Summary: desc, Kind: "skill", Scope: scope, Entry: p, Source: source, Path: p})
	}
	return docs
}

// frontmatter returns the name and description from a SKILL.md YAML header. It handles plain,
// quoted, folded (>) and literal (|) scalars, which is all skill headers use.
func frontmatter(path string) (name, desc string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 16<<10))
	if err != nil {
		return "", "", false
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 4096), 16<<10)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", "", false
	}
	vals := map[string]string{}
	key := ""
	var cont []string
	flush := func() {
		if key != "" && len(cont) > 0 {
			vals[key] = strings.TrimSpace(vals[key] + " " + strings.Join(cont, " "))
		}
		key, cont = "", nil
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if t := strings.TrimSpace(line); t == "---" || t == "..." {
			break
		}
		if line != "" && (line[0] == ' ' || line[0] == '\t') {
			if key != "" {
				cont = append(cont, strings.TrimSpace(line))
			}
			continue
		}
		flush()
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		if k != "name" && k != "description" {
			continue
		}
		key = k
		v = strings.TrimSpace(v)
		switch {
		case v == "" || strings.HasPrefix(v, "|") || strings.HasPrefix(v, ">"):
			v = ""
		case strings.HasPrefix(v, `"`):
			if u, err := strconv.Unquote(v); err == nil {
				v = u
			} else {
				v = strings.Trim(v, `"`)
			}
		case strings.HasPrefix(v, "'"):
			v = strings.ReplaceAll(strings.Trim(v, "'"), "''", "'")
		}
		vals[k] = v
	}
	flush()
	return vals["name"], vals["description"], true
}

type wantedFile struct {
	Schema   string   `json:"schema"`
	Families []Family `json:"families"`
}

// readWanted reads registry/wanted.json; it is the root marker, so a broken file is an error.
func readWanted(path string, st *[]stamp) ([]*doc, error) {
	*st = append(*st, stampOf(path))
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unreadable %s: %w", path, err)
	}
	var w wantedFile
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if w.Schema != WantedSchema {
		return nil, fmt.Errorf("%s: schema %q, want %q", path, w.Schema, WantedSchema)
	}
	sort.SliceStable(w.Families, func(i, j int) bool { return w.Families[i].Family < w.Families[j].Family })
	docs := make([]*doc, 0, len(w.Families))
	for i := range w.Families {
		f := w.Families[i]
		if f.Family == "" {
			continue
		}
		docs = append(docs, &doc{
			Name:    f.Family,
			Summary: f.Evidence,
			Intents: f.Intents,
			Tags:    []string{f.SuggestedName},
			Kind:    "wanted",
			Scope:   "global",
			Entry:   f.SuggestedName,
			Source:  srcWanted,
			Path:    path,
			Wanted:  &f,
		})
	}
	return docs, nil
}

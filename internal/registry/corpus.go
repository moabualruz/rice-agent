package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The corpus is one markdown card per rice-agent tool, rice-agent skill, external provider and
// wanted family under <StateDir>/corpus/<kind>/<name>.md (kinds tool, skill, provider, wanted).
// It is rewritten whenever the cached index is rebuilt and is what an explicitly created
// zvec-grep index covers (index --semantic).
const (
	CorpusDir     = "corpus"
	zvecIndexDir  = ".zvec-grep"       // where zvec-grep keeps a workspace index, inside its root
	refreshMarker = ".refresh-pending" // hidden, so zvec-grep does not index it
)

// Card kinds, one directory each.
const (
	cardTool     = "tool"
	cardSkill    = "skill"
	cardExternal = "provider" // not "external": zvec-grep skips such dependency-like directories
	cardWanted   = "wanted"
)

var cardKinds = []string{cardTool, cardSkill, cardExternal, cardWanted}

func (ix *Index) corpusDir() string { return filepath.Join(ix.stateDir, CorpusDir) }

// cardKind is the corpus directory for d, or "" when d gets no card (project commands, installed
// skills and hidden tools).
func cardKind(d *doc) string {
	switch {
	case d.Hidden:
		return ""
	case d.Tool != nil:
		return cardTool
	case d.Provider != nil:
		return cardExternal
	case d.Wanted != nil:
		return cardWanted
	case d.Source == srcAgentDry && d.Kind == "skill":
		return cardSkill
	}
	return ""
}

// cardFile turns a name into a file name: letters, digits, dot, underscore and hyphen are kept.
func cardFile(name string) string {
	b := []byte(name)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	s := strings.Trim(string(b), ".")
	if s == "" {
		s = "-"
	}
	return s + ".md"
}

// cardRel is d's card path relative to the corpus directory, or "".
func cardRel(d *doc) string {
	k := cardKind(d)
	if k == "" {
		return ""
	}
	return k + "/" + cardFile(d.Name)
}

// cardText is the markdown card: the name as the heading, then kind, entry, summary and intents.
// Paths are relative to the rice-agent root so cards do not change when the root moves.
func cardText(d *doc, root string) string {
	rel := func(p string) string {
		if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
		return p
	}
	var kind, entry string
	switch {
	case d.Tool != nil:
		kind = d.Kind + " " + d.Scope + " (rice-agent tool)"
		entry = d.Name + " (" + rel(d.Tool.EntryPath()) + ")"
	case d.Provider != nil:
		p := d.Provider
		kind = "external global (CLI from " + p.Owner + ", " + p.License + ")"
		entry = p.Command + " (install: " + p.Install + ")"
	case d.Wanted != nil:
		kind = "wanted global (no tool yet)"
		entry = "build " + d.Wanted.SuggestedName + " (" + d.Wanted.SuggestedKind + ")"
	default:
		kind = "skill " + d.Scope + " (rice-agent skill)"
		entry = rel(d.Entry)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", d.Name)
	line := func(k, v string) {
		if v = strings.Join(strings.Fields(v), " "); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	line("kind", kind)
	line("entry", entry)
	line("summary", d.Summary)
	line("intents", strings.Join(d.Intents, "; "))
	if d.Provider != nil {
		line("notes", d.Provider.Notes)
	}
	return b.String()
}

// writeCorpus writes a card for every eligible document, leaving unchanged cards untouched so an
// incremental index refresh sees only real edits, and removes cards whose entry disappeared. It
// reports whether any card was written or removed and how many cards the corpus holds.
func writeCorpus(dir, root string, docs []*doc) (changed bool, cards int, err error) {
	want := map[string]string{}
	var order []string
	for _, d := range docs {
		rel := cardRel(d)
		if rel == "" {
			continue
		}
		if _, dup := want[rel]; dup {
			continue // first one wins, as in lookups
		}
		want[rel] = cardText(d, root)
		order = append(order, rel)
	}
	for _, k := range cardKinds {
		if err := os.MkdirAll(filepath.Join(dir, k), 0o700); err != nil {
			return changed, 0, err
		}
	}
	sort.Strings(order)
	for _, rel := range order {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, []byte(want[rel])) {
			continue
		}
		if err := writeAtomic(p, []byte(want[rel])); err != nil {
			return changed, 0, err
		}
		changed = true
	}
	for _, k := range cardKinds {
		entries, err := os.ReadDir(filepath.Join(dir, k))
		if err != nil {
			return changed, 0, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if _, keep := want[k+"/"+e.Name()]; keep {
				continue
			}
			if err := os.Remove(filepath.Join(dir, k, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return changed, 0, err
			}
			changed = true
		}
	}
	return changed, len(want), nil
}

// writeAtomic replaces p through a temporary file in the same directory.
func writeAtomic(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".card-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// countCards counts the cards on disk.
func countCards(dir string) int {
	n := 0
	for _, k := range cardKinds {
		matches, _ := filepath.Glob(filepath.Join(dir, k, "*.md"))
		n += len(matches)
	}
	return n
}

// semanticIndexed reports whether an explicitly created zvec-grep index covers the corpus.
func semanticIndexed(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, zvecIndexDir))
	return err == nil && info.IsDir()
}

// pendingState reads the refresh marker: whether a refresh is pending and, when the last
// automatic attempt failed, why.
func pendingState(dir string) (pending bool, failure string) {
	raw, err := os.ReadFile(filepath.Join(dir, refreshMarker))
	if err != nil {
		return false, ""
	}
	return true, strings.TrimSpace(string(raw))
}

func markPending(dir, failure string) error {
	return os.WriteFile(filepath.Join(dir, refreshMarker), []byte(failure), 0o600)
}

func clearPending(dir string) {
	os.Remove(filepath.Join(dir, refreshMarker))
}

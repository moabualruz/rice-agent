package registry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Project sources.
const (
	srcJust     = "project:just"
	srcNPM      = "project:npm"
	srcMake     = "project:make"
	srcScripts  = "project:scripts"
	srcProjFile = "project:agent-dry"
	// ProjectFile is the optional per-project list of extra commands.
	ProjectFile = ".agent-dry.json"
)

// projectRoot is an explicit --project root as given, or the Git root of the working directory.
func projectRoot(p string) string {
	if p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return ""
		}
		return abs
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return gitRoot(wd)
}

// gitRoot walks up from dir to the first directory holding .git (a directory, or a file for
// worktrees and submodules). It does not run git.
func gitRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// discoverProject reads the project's commands and skills live.
func discoverProject(root string, getenv func(string) string) (docs, skills []*doc, warns []string) {
	jd, w := justRecipes(root, getenv)
	docs = append(docs, jd...)
	warns = append(warns, w...)
	docs = append(docs, npmScripts(root)...)
	docs = append(docs, makeTargets(root)...)
	docs = append(docs, projectScripts(root)...)
	pd, w := projectFile(root)
	docs = append(docs, pd...)
	warns = append(warns, w...)
	skills = append(readSkills(filepath.Join(root, ".agents", "skills"), nil, srcSkill, "project"),
		readSkills(filepath.Join(root, ".claude", "skills"), nil, srcSkill, "project")...)
	return docs, skills, warns
}

func firstFile(root string, names ...string) string {
	for _, n := range names {
		p := filepath.Join(root, n)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

type justDump struct {
	Recipes map[string]struct {
		Doc     *string `json:"doc"`
		Private bool    `json:"private"`
	} `json:"recipes"`
	Modules map[string]justDump `json:"modules"`
}

// justRecipes uses `just --dump --dump-format json` when just is on PATH, and a line parser of the
// justfile otherwise (Hetzner has no just).
func justRecipes(root string, getenv func(string) string) ([]*doc, []string) {
	file := firstFile(root, "justfile", "Justfile", ".justfile", "JUSTFILE")
	if file == "" {
		return nil, nil
	}
	recipes := map[string]string{}
	var warns []string
	if jp := lookPath(getenv, "just"); jp != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, jp, "--justfile", file, "--working-directory", root, "--dump", "--dump-format", "json")
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		var dump justDump
		if err == nil {
			err = json.Unmarshal(out, &dump)
		}
		if err == nil {
			flattenJust(dump, "", recipes)
		} else {
			warns = append(warns, "just --dump failed for "+file+"; parsed the justfile directly")
			recipes = parseJustfile(file)
		}
	} else {
		recipes = parseJustfile(file)
	}
	names := make([]string, 0, len(recipes))
	for n := range recipes {
		names = append(names, n)
	}
	sort.Strings(names)
	docs := make([]*doc, 0, len(names))
	for _, n := range names {
		docs = append(docs, &doc{Name: n, Summary: recipes[n], Kind: "recipe", Scope: "project", Entry: "just " + n, Source: srcJust, Path: file})
	}
	return docs, warns
}

func flattenJust(d justDump, prefix string, out map[string]string) {
	for name, r := range d.Recipes {
		if r.Private || strings.HasPrefix(name, "_") {
			continue
		}
		doc := ""
		if r.Doc != nil {
			doc = *r.Doc
		}
		out[prefix+name] = doc
	}
	for mod, sub := range d.Modules {
		flattenJust(sub, prefix+mod+"::", out)
	}
}

var reJustRecipe = regexp.MustCompile(`^@?([A-Za-z_][A-Za-z0-9_-]*)(?:\s+[^:]*)?:(?:[^=]|$)`)

// parseJustfile finds top-level recipe headers and the comment line directly above each.
func parseJustfile(file string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(file)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	comment, private := "", false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "" || line[0] == ' ' || line[0] == '\t':
			if line == "" {
				comment, private = "", false
			}
			continue
		case strings.HasPrefix(line, "#"):
			comment = strings.TrimSpace(strings.TrimLeft(line, "#"))
			continue
		case strings.HasPrefix(line, "["):
			if strings.Contains(line, "private") {
				private = true
			}
			continue
		}
		for _, kw := range []string{"set ", "export ", "alias ", "import ", "mod ", "import?", "mod?"} {
			if strings.HasPrefix(line, kw) {
				line = ""
			}
		}
		if m := reJustRecipe.FindStringSubmatch(line); m != nil && !strings.Contains(line, ":=") {
			if !private && !strings.HasPrefix(m[1], "_") {
				if _, dup := out[m[1]]; !dup {
					out[m[1]] = comment
				}
			}
		}
		comment, private = "", false
	}
	return out
}

func npmScripts(root string) []*doc {
	file := filepath.Join(root, "package.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return nil
	}
	runner := "npm run"
	switch {
	case firstFile(root, "pnpm-lock.yaml") != "":
		runner = "pnpm run"
	case firstFile(root, "yarn.lock") != "":
		runner = "yarn"
	case firstFile(root, "bun.lock", "bun.lockb") != "":
		runner = "bun run"
	}
	names := make([]string, 0, len(pkg.Scripts))
	for n := range pkg.Scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	docs := make([]*doc, 0, len(names))
	for _, n := range names {
		body := pkg.Scripts[n]
		docs = append(docs, &doc{Name: n, Summary: body, Kind: "recipe", Scope: "project", Entry: runner + " " + n, Source: srcNPM, Path: file, Command: body})
	}
	return docs
}

var reMakeTarget = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_./-]*(?:[ \t]+[A-Za-z0-9][A-Za-z0-9_./-]*)*)[ \t]*:(?:[^=]|$)(.*)$`)

// makeTargets reads explicit targets with the `## doc` suffix or the comment line above them.
func makeTargets(root string) []*doc {
	file := firstFile(root, "GNUmakefile", "makefile", "Makefile")
	if file == "" {
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	var docs []*doc
	seen := map[string]bool{}
	comment := ""
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || line[0] == '\t' || line[0] == ' ' {
			comment = ""
			continue
		}
		if strings.HasPrefix(line, "#") {
			comment = strings.TrimSpace(strings.TrimLeft(line, "#"))
			continue
		}
		m := reMakeTarget.FindStringSubmatch(line)
		if m == nil || strings.Contains(line, "::=") || strings.Contains(m[1], "%") {
			comment = ""
			continue
		}
		summary := comment
		if _, after, ok := strings.Cut(m[2], "##"); ok {
			summary = strings.TrimSpace(after)
		}
		for _, t := range strings.Fields(m[1]) {
			if strings.HasPrefix(t, ".") || seen[t] {
				continue
			}
			seen[t] = true
			docs = append(docs, &doc{Name: t, Summary: summary, Kind: "recipe", Scope: "project", Entry: "make " + t, Source: srcMake, Path: file})
		}
		comment = ""
	}
	return docs
}

// projectScripts lists executable files in <root>/scripts with the first comment line after the
// shebang as the summary.
func projectScripts(root string) []*doc {
	dir := filepath.Join(root, "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var docs []*doc
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		docs = append(docs, &doc{Name: name, Summary: leadingComment(p), Kind: "script", Scope: "project", Entry: "scripts/" + e.Name(), Source: srcScripts, Path: p})
	}
	return docs
}

func leadingComment(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, _ := io.ReadAll(io.LimitReader(f, 4096))
	if bytes.IndexByte(raw, 0) >= 0 {
		return "" // binary
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if i == 0 && strings.HasPrefix(line, "#!") {
			continue
		}
		var text string
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "//"):
			text = strings.TrimSpace(strings.TrimLeft(line, "/"))
		case strings.HasPrefix(line, "#"):
			text = strings.TrimSpace(strings.TrimLeft(line, "#"))
		default:
			return ""
		}
		low := strings.ToLower(text)
		if text == "" || strings.HasPrefix(low, "shellcheck") || strings.HasPrefix(low, "@ts-") || strings.HasPrefix(low, "-*-") {
			continue
		}
		return text
	}
	return ""
}

type projectEntry struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Intents []string `json:"intents"`
	Entry   string   `json:"entry"`
	Kind    string   `json:"kind,omitempty"`
}

// projectFile reads the optional .agent-dry.json array of {name, summary, intents, entry}.
func projectFile(root string) ([]*doc, []string) {
	file := filepath.Join(root, ProjectFile)
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{"unreadable " + file}
	}
	var entries []projectEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, []string{file + ": " + err.Error()}
	}
	var docs []*doc
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		kind := e.Kind
		if kind == "" {
			kind = "recipe"
		}
		entry := e.Entry
		if entry == "" {
			entry = e.Name
		}
		docs = append(docs, &doc{Name: e.Name, Summary: e.Summary, Intents: e.Intents, Kind: kind, Scope: "project", Entry: entry, Source: srcProjFile, Path: file})
	}
	return docs, nil
}

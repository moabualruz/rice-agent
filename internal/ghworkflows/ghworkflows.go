// Package ghworkflows holds the repository's GitHub Actions files to the public-repository policy:
// only GitHub-hosted runners, no trigger that runs base-repository code on fork input, no
// reusable workflows, read-only permissions and no secrets. The YAML is parsed, so quoted keys and
// flow-style mappings are judged the same as block style. A workflow that truly needs more should
// change this policy on purpose, in review.
package ghworkflows

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	hostedRunner = regexp.MustCompile(`^(ubuntu|windows|macos)-(latest|[0-9]{2}(\.[0-9]+)?(-arm|-intel|-xlarge)?|20[0-9]{2})$`)
	secretsWord  = regexp.MustCompile(`(?i)\bsecrets\b`)
	selfHosted   = regexp.MustCompile(`(?i)self-hosted`)
	badTriggers  = map[string]bool{"pull_request_target": true, "workflow_run": true, "issue_comment": true}
)

// Files lists the workflow files under .github/workflows and every action.yml or action.yaml
// under .github/actions, recursively.
func Files(root string) ([]string, error) {
	var out []string
	for _, pat := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml"} {
		m, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat)))
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	actions := filepath.Join(root, ".github", "actions")
	if _, err := os.Stat(actions); err == nil {
		err := filepath.WalkDir(actions, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && (d.Name() == "action.yml" || d.Name() == "action.yaml") {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// Check returns one message per policy violation in the workflow or action file at path.
func Check(path string, data []byte) []string {
	var v []string
	add := func(format string, a ...any) { v = append(v, path+": "+fmt.Sprintf(format, a...)) }

	text := string(data)
	if selfHosted.MatchString(text) {
		add("self-hosted runners are not allowed")
	}
	if secretsWord.MatchString(text) {
		add("secrets are not allowed")
	}

	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		add("not valid YAML: %v", err)
		return v
	}
	top, ok := norm(doc).(map[string]any)
	if !ok {
		add("top level must be a mapping")
		return v
	}

	if on, present := top["on"]; present {
		for _, t := range triggers(on) {
			if badTriggers[t] {
				add("trigger %s is not allowed", t)
			}
		}
	}

	jobs, isWorkflow := top["jobs"]
	if !isWorkflow {
		return v
	}
	jobMap, ok := jobs.(map[string]any)
	if !ok {
		add("jobs must be a mapping")
		return v
	}
	_, workflowPerms := top["permissions"]
	if workflowPerms {
		checkPermissions(top["permissions"], "workflow", add)
	}
	names := make([]string, 0, len(jobMap))
	for n := range jobMap {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		job, ok := jobMap[name].(map[string]any)
		if !ok {
			add("job %s must be a mapping", name)
			continue
		}
		if _, reusable := job["uses"]; reusable {
			add("job %s calls a reusable workflow, which is not allowed", name)
		}
		if perms, present := job["permissions"]; present {
			checkPermissions(perms, "job "+name, add)
		} else if !workflowPerms {
			add("job %s has no permissions and the workflow sets none", name)
		}
		checkRunsOn(name, job["runs-on"], add)
	}
	return v
}

func checkRunsOn(job string, runsOn any, add func(string, ...any)) {
	switch r := runsOn.(type) {
	case string:
		if !hostedRunner.MatchString(r) {
			add("job %s runs-on %q is not a GitHub-hosted runner label", job, r)
		}
	case []any:
		if len(r) == 0 {
			add("job %s runs-on is empty", job)
		}
		for _, e := range r {
			s, ok := e.(string)
			if !ok || !hostedRunner.MatchString(s) {
				add("job %s runs-on entry %v is not a GitHub-hosted runner label", job, e)
			}
		}
	case nil:
		add("job %s has no runs-on", job)
	default:
		add("job %s runs-on must be a hosted runner label, not %T", job, runsOn)
	}
}

func checkPermissions(p any, where string, add func(string, ...any)) {
	switch x := p.(type) {
	case string:
		if x != "read-all" {
			add("%s permissions %q is not allowed; use read or none per scope", where, x)
		}
	case map[string]any:
		for scope, level := range x {
			if s, _ := level.(string); strings.ToLower(s) != "read" && strings.ToLower(s) != "none" {
				add("%s permission %s: %v must be read or none", where, scope, level)
			}
		}
	default:
		add("%s permissions must be read-all or a mapping of read or none", where)
	}
}

func triggers(on any) []string {
	var out []string
	switch x := on.(type) {
	case string:
		out = append(out, strings.ToLower(x))
	case []any:
		for _, e := range x {
			out = append(out, strings.ToLower(fmt.Sprint(e)))
		}
	case map[string]any:
		for k := range x {
			out = append(out, strings.ToLower(k))
		}
	}
	return out
}

// norm turns every decoded mapping into map[string]any with lowercase-insensitive stringified
// keys kept as written, except that YAML 1.1 booleans used as keys (on, y, n) become their text.
func norm(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = norm(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			key := fmt.Sprint(k)
			if b, ok := k.(bool); ok && b {
				key = "on"
			}
			out[key] = norm(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = norm(e)
		}
		return out
	}
	return v
}

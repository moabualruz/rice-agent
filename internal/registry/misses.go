package registry

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MissFile is the miss log name inside StateDir.
const MissFile = "misses.jsonl"

// seenSimilarity is the Jaccard similarity at which an earlier miss is reported as SEEN.
const seenSimilarity = 0.5

// missTail bounds how much of the miss log a lookup reads (the most recent records).
const missTail = 1 << 20

// miss is one line of misses.jsonl.
type miss struct {
	TS      string   `json:"ts"`
	Query   string   `json:"query"`
	Tokens  []string `json:"tokens"`
	Project string   `json:"project"`
	Harness string   `json:"harness"`
	Session string   `json:"session,omitempty"`
}

func (ix *Index) missPath() string { return filepath.Join(ix.stateDir, MissFile) }

// harness names the calling agent harness from its environment.
func harness(getenv func(string) string) string {
	has := func(prefix string, known ...string) bool {
		for _, k := range known {
			if getenv(k) != "" {
				return true
			}
		}
		// Variables present in the real environment, confirmed through getenv so a test's
		// substitute environment decides.
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(name, prefix) && getenv(name) != "" {
				return true
			}
		}
		return false
	}
	switch {
	case getenv("CLAUDECODE") != "":
		return "claude"
	case has("CODEX_", "CODEX_HOME", "CODEX_SANDBOX", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_CI", "CODEX_MANAGED_BY_NPM"):
		return "codex"
	case getenv("GEMINI_CLI") != "":
		return "gemini"
	case has("HERMES", "HERMES_HOME", "HERMES_SESSION_ID", "HERMES_AGENT"):
		return "hermes"
	}
	return "unknown"
}

// session is a session reference from the harness environment, when one is present.
func session(getenv func(string) string) string {
	for _, k := range []string{"CLAUDE_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID", "GEMINI_SESSION_ID", "HERMES_SESSION_ID"} {
		if v := getenv(k); v != "" {
			return truncate(v, 128)
		}
	}
	return ""
}

// recordMiss appends one redacted record with a single O_APPEND write.
func (ix *Index) recordMiss(query string, now time.Time) error {
	q := redact(query)
	rec := miss{
		TS:      now.UTC().Format(time.RFC3339),
		Query:   truncate(q, 300),
		Tokens:  signature(q),
		Project: ix.projectName,
		Harness: harness(ix.getenv),
		Session: session(ix.getenv),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <redacted> readable
	if err := enc.Encode(rec); err != nil {
		return err
	}
	line := buf.Bytes() // Encode ends the record with a newline
	if err := os.MkdirAll(ix.stateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(ix.missPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// seen returns the most recent earlier miss whose signature has Jaccard similarity of at least
// 0.5 with the query, or nil.
func (ix *Index) seen(query string) *miss {
	sig := signature(redact(query))
	if len(sig) == 0 {
		return nil
	}
	f, err := os.Open(ix.missPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	off := int64(0)
	if info.Size() > missTail {
		off = info.Size() - missTail
	}
	raw, err := io.ReadAll(io.NewSectionReader(f, off, info.Size()-off))
	if err != nil {
		return nil
	}
	lines := bytes.Split(raw, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // partial first line
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if len(bytes.TrimSpace(lines[i])) == 0 {
			continue
		}
		var m miss
		if json.Unmarshal(lines[i], &m) != nil {
			continue
		}
		if jaccard(sig, m.Tokens) >= seenSimilarity {
			return &m
		}
	}
	return nil
}

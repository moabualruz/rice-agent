package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// transcript is one harness session file. Session id is the file stem.
type transcript struct {
	path     string
	session  string
	subagent bool
	harness  string
	mtime    time.Time
}

// discover lists <dir>/*/*.jsonl (main sessions) and <dir>/*/*/subagents/*.jsonl modified at or
// after cutoff. Main sessions come first, newest first, then subagents newest first, so that a
// --max-files bound keeps every main session before any subagent (the evidence study's method).
// It returns the selected files and how many in-window files the bound left out.
func discover(dir string, cutoff time.Time, maxFiles int) ([]transcript, int, error) {
	projects, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var mains, subs []transcript
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		pdir := filepath.Join(dir, p.Name())
		entries, err := os.ReadDir(pdir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				sdir := filepath.Join(pdir, e.Name(), "subagents")
				files, err := os.ReadDir(sdir)
				if err != nil {
					continue
				}
				for _, f := range files {
					if t, ok := candidate(sdir, f, cutoff, true); ok {
						subs = append(subs, t)
					}
				}
				continue
			}
			if t, ok := candidate(pdir, e, cutoff, false); ok {
				mains = append(mains, t)
			}
		}
	}
	newest := func(ts []transcript) {
		sort.Slice(ts, func(i, j int) bool {
			if !ts[i].mtime.Equal(ts[j].mtime) {
				return ts[i].mtime.After(ts[j].mtime)
			}
			return ts[i].path < ts[j].path
		})
	}
	newest(mains)
	newest(subs)
	all := append(mains, subs...)
	left := 0
	if maxFiles > 0 && len(all) > maxFiles {
		left = len(all) - maxFiles
		all = all[:maxFiles]
	}
	return all, left, nil
}

func candidate(dir string, e os.DirEntry, cutoff time.Time, sub bool) (transcript, bool) {
	if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
		return transcript{}, false
	}
	info, err := e.Info()
	if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(cutoff) {
		return transcript{}, false
	}
	return transcript{
		path:     filepath.Join(dir, e.Name()),
		session:  strings.TrimSuffix(e.Name(), ".jsonl"),
		subagent: sub,
		harness:  "claude",
		mtime:    info.ModTime(),
	}, true
}

// event is one tool_use from an assistant line. cmd lives only in memory and is never printed.
type event struct {
	id      string
	tool    string
	cmd     string
	at      time.Time
	harness string
	host    string
}

// result is the outcome of a tool_use, joined by id.
type result struct {
	id      string
	isError bool
}

var (
	markToolUse    = []byte(`"type":"tool_use"`)
	markAssistant  = []byte(`"type":"assistant"`)
	markToolResult = []byte(`"type":"tool_result"`)
	markUser       = []byte(`"type":"user"`)
)

type assistantLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// scanFile streams one transcript. onEvent receives tool_use blocks from assistant lines whose
// timestamp is not before cutoff (lines without a timestamp count). wantResult reports whether a
// tool_use id still waits for its result; onResult receives those results. Other line types
// (api-request-blob replays, attachments, queue operations) are ignored.
func scanFile(path string, cutoff time.Time, onEvent func(event), wantResult func(line []byte) bool, onResult func(result)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return eachLine(f, func(line []byte) {
		switch {
		case bytes.Contains(line, markToolUse) && bytes.Contains(line, markAssistant):
			var l assistantLine
			if json.Unmarshal(line, &l) != nil || l.Type != "assistant" {
				return
			}
			at := time.Time{}
			if l.Timestamp != "" {
				if ts, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
					at = ts
					if ts.Before(cutoff) {
						return
					}
				}
			}
			var blocks []contentBlock
			if json.Unmarshal(l.Message.Content, &blocks) != nil {
				return
			}
			for _, b := range blocks {
				if b.Type != "tool_use" || b.Name == "" {
					continue
				}
				ev := event{id: b.ID, tool: b.Name, at: at, harness: "claude"}
				if b.Name == "Bash" {
					var in struct {
						Command string `json:"command"`
					}
					_ = json.Unmarshal(b.Input, &in)
					ev.cmd = in.Command
				}
				onEvent(ev)
			}
		case bytes.Contains(line, markToolResult) && bytes.Contains(line, markUser) && wantResult(line):
			var l assistantLine
			if json.Unmarshal(line, &l) != nil || l.Type != "user" {
				return
			}
			var blocks []contentBlock
			if json.Unmarshal(l.Message.Content, &blocks) != nil {
				return
			}
			for _, b := range blocks {
				if b.Type == "tool_result" && b.ToolUseID != "" {
					onResult(result{id: b.ToolUseID, isError: b.IsError})
				}
			}
		}
	})
}

// eachLine calls fn for every line, reusing one buffer; lines can be tens of megabytes.
func eachLine(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			buf = append(buf, chunk...)
			continue
		}
		line := chunk
		if len(buf) > 0 {
			buf = append(buf, chunk...)
			line = buf
		}
		if len(line) > 0 {
			fn(line)
		}
		buf = buf[:0]
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// discoverCodex walks the official sessions/YYYY/MM/DD layout and also accepts YYYY/MM/*.jsonl.
func discoverCodex(dir string, cutoff time.Time) ([]transcript, error) {
	var out []transcript
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(cutoff) {
			return nil
		}
		out = append(out, transcript{
			path:    path,
			session: strings.TrimSuffix(entry.Name(), ".jsonl"),
			harness: "codex",
			mtime:   info.ModTime(),
		})
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].mtime.Equal(out[j].mtime) {
			return out[i].mtime.After(out[j].mtime)
		}
		return out[i].path < out[j].path
	})
	return out, nil
}

func limitTranscripts(files []transcript, maxFiles int) ([]transcript, int) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].subagent != files[j].subagent {
			return !files[i].subagent
		}
		if !files[i].mtime.Equal(files[j].mtime) {
			return files[i].mtime.After(files[j].mtime)
		}
		return files[i].path < files[j].path
	})
	if maxFiles <= 0 || len(files) <= maxFiles {
		return files, 0
	}
	left := len(files) - maxFiles
	return files[:maxFiles], left
}

// scanCodexFile reads the current Codex rollout JSONL format, where function-call arguments are
// themselves JSON strings. It retains only command text in memory for classification.
func scanCodexFile(path string, cutoff time.Time, onEvent func(event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return eachLine(f, func(line []byte) {
		var record struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type      string          `json:"type"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				CallID    string          `json:"call_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil || record.Type != "response_item" || record.Payload.Type != "function_call" {
			return
		}
		at, err := time.Parse(time.RFC3339Nano, record.Timestamp)
		if err == nil && at.Before(cutoff) {
			return
		}
		if record.Payload.Name != "exec_command" {
			return
		}
		var args struct {
			Cmd     string `json:"cmd"`
			Command string `json:"command"`
		}
		var raw string
		if json.Unmarshal(record.Payload.Arguments, &raw) == nil {
			_ = json.Unmarshal([]byte(raw), &args)
		} else {
			_ = json.Unmarshal(record.Payload.Arguments, &args)
		}
		cmd := args.Cmd
		if cmd == "" {
			cmd = args.Command
		}
		if cmd == "" {
			return
		}
		if err != nil {
			at = time.Time{}
		}
		onEvent(event{
			id:      "codex:" + record.Payload.CallID,
			tool:    "Bash",
			cmd:     cmd,
			at:      at,
			harness: "codex",
		})
	})
}

package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// transcript is one Claude Code session file. Session id is the file stem.
type transcript struct {
	path     string
	session  string
	subagent bool
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
		mtime:    info.ModTime(),
	}, true
}

// event is one tool_use from an assistant line. cmd lives only in memory and is never printed.
type event struct {
	id   string
	tool string
	cmd  string
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
			if l.Timestamp != "" {
				if ts, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil && ts.Before(cutoff) {
					return
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
				ev := event{id: b.ID, tool: b.Name}
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

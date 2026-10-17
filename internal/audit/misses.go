package audit

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"time"
)

// missSummary reports the find miss log. Only normalized tokens are printed, never queries.
type missSummary struct {
	Present   bool       `json:"present"`
	Total     int        `json:"total"`
	InWindow  int        `json:"in_window"`
	Malformed int        `json:"malformed,omitempty"`
	Top       []tokenSet `json:"top"`
}

type tokenSet struct {
	Tokens []string `json:"tokens"`
	Count  int      `json:"count"`
}

// missTopN is how many token sets the report lists.
const missTopN = 5

// summarizeMisses reads misses.jsonl. find writes {ts, query, tokens, project, harness, session};
// this reader uses only the time and the token set, never the query. It also accepts the token set
// under signature, keywords, words or terms (array or space-separated string) and the time under
// date, time or timestamp (YYYY-MM-DD or RFC 3339). A line without a readable time counts.
func summarizeMisses(path string, cutoff time.Time) (missSummary, error) {
	var s missSummary
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	defer f.Close()
	s.Present = true
	counts := map[string]int{}
	err = eachLine(f, func(line []byte) {
		if len(strings.TrimSpace(string(line))) == 0 {
			return
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(line, &m) != nil {
			s.Malformed++
			return
		}
		s.Total++
		if t, ok := missTime(m); ok && t.Before(cutoff) {
			return
		}
		s.InWindow++
		toks := missTokens(m)
		if len(toks) > 0 {
			counts[strings.Join(toks, " ")]++
		}
	})
	if err != nil {
		return s, err
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > missTopN {
		keys = keys[:missTopN]
	}
	for _, k := range keys {
		s.Top = append(s.Top, tokenSet{Tokens: strings.Fields(k), Count: counts[k]})
	}
	return s, nil
}

func missTime(m map[string]json.RawMessage) (time.Time, bool) {
	for _, k := range []string{"date", "ts", "time", "timestamp"} {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// missTokens returns the sorted, unique, redacted token set of one miss.
func missTokens(m map[string]json.RawMessage) []string {
	for _, k := range []string{"tokens", "signature", "keywords", "words", "terms"} {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var list []string
		if json.Unmarshal(raw, &list) != nil {
			var str string
			if json.Unmarshal(raw, &str) != nil {
				continue
			}
			list = strings.Fields(str)
		}
		seen := map[string]bool{}
		var out []string
		for _, t := range list {
			t = redactToken(strings.ToLower(strings.TrimSpace(t)))
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

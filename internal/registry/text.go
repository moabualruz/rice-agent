package registry

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// stopwords never carry intent. "use" and "when" are here because every manifest summary
// starts with "Use when".
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "for": true, "of": true, "in": true, "on": true,
	"at": true, "by": true, "with": true, "from": true, "into": true, "onto": true, "and": true,
	"or": true, "but": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "it": true, "its": true, "this": true, "that": true, "these": true, "those": true,
	"my": true, "me": true, "i": true, "we": true, "our": true, "you": true, "your": true,
	"use": true, "when": true, "how": true, "what": true, "which": true, "do": true, "does": true,
	"can": true, "should": true, "please": true, "all": true, "any": true, "some": true, "via": true,
	"using": true, "as": true, "if": true, "then": true, "so": true, "no": true, "not": true,
	"there": true, "here": true, "need": true, "want": true, "about": true,
}

// tokenize lowercases s, splits on every rune that is not a letter or digit (so names split on
// - and _), drops stopwords and single letters, and strips plural endings.
func tokenize(s string) []string {
	var out []string
	s = strings.ToLower(s)
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		w := s[start:end]
		start = -1
		if stopwords[w] {
			return
		}
		w = stem(w)
		if len(w) < 2 && !(len(w) == 1 && w[0] >= '0' && w[0] <= '9') {
			return
		}
		if stopwords[w] {
			return
		}
		out = append(out, w)
	}
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(s))
	return out
}

// stem is a light plural normalizer: dashes -> dash, checks -> check, policies -> policy.
func stem(w string) string {
	n := len(w)
	if n <= 3 {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ies") && n > 4:
		return w[:n-3] + "y"
	case strings.HasSuffix(w, "sses"):
		return w[:n-2]
	case strings.HasSuffix(w, "shes"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "zes"):
		return w[:n-2]
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
		return w
	case strings.HasSuffix(w, "s"):
		return w[:n-1]
	}
	return w
}

// signature is the sorted, unique token set of a query; the miss log stores it.
func signature(query string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tokenize(query) {
		if t == "redacted" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// jaccard is |a and b| / |a or b| over two sorted unique token lists.
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]bool, len(a))
	for _, t := range a {
		set[t] = true
	}
	inter := 0
	union := len(a)
	for _, t := range b {
		if set[t] {
			inter++
		} else {
			union++
		}
	}
	return float64(inter) / float64(union)
}

const redacted = "<redacted>"

var (
	reBearer   = regexp.MustCompile(`(?i)\b(bearer)(\s+)\S+`)
	reAssign   = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|access[_-]?key)(\s*[=:]\s*)\S+`)
	reKnown    = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|glpat-[A-Za-z0-9_-]+|hv[sbr]\.[A-Za-z0-9_.-]+|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]+|sk-[A-Za-z0-9_-]{16,})`)
	reLongRun  = regexp.MustCompile(`[A-Za-z0-9+/=_-]{24,}`)
	reHasDigit = regexp.MustCompile(`[0-9]`)
)

// redact replaces token-shaped words with <redacted> before a query is printed or logged:
// known token prefixes (ghp_, gho_, ghs_, github_pat_, hvs., ...), Bearer values, key=value
// secrets, and runs of 24 or more base64 or hex characters that contain a digit.
func redact(s string) string {
	s = reBearer.ReplaceAllString(s, "${1}${2}"+redacted)
	s = reAssign.ReplaceAllString(s, "${1}${2}"+redacted)
	s = reKnown.ReplaceAllString(s, redacted)
	s = reLongRun.ReplaceAllStringFunc(s, func(m string) string {
		if reHasDigit.MatchString(m) {
			return redacted
		}
		return m
	})
	return s
}

// truncate shortens s to at most n runes, ending with "..." when cut.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return strings.TrimRight(string(r[:n-3]), " ") + "..."
}

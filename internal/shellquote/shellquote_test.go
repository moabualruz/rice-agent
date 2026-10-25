package shellquote

import "testing"

func TestPOSIX(t *testing.T) {
	cases := map[string]string{
		"":                 "''",
		"path with spaces": "'path with spaces'",
		"a'b":              "'a'\\''b'",
	}
	for input, want := range cases {
		if got := POSIX(input); got != want {
			t.Errorf("POSIX(%q) = %q, want %q", input, got, want)
		}
	}
}

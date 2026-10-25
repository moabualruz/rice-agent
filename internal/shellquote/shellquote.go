package shellquote

import "strings"

// POSIX returns s as one safely quoted argument for a POSIX shell.
func POSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

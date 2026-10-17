// Package gates scaffolds new tools and runs the admission gates G1 to G6.
// It registers the new and check commands.
package gates

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/registry"
)

func init() {
	cli.Register(cli.Command{Name: "new", Summary: "duplicate check, then scaffold a tool, script or skill", Run: runNew})
	cli.Register(cli.Command{Name: "check", Summary: "run admission gates G1 to G6 on tools", Run: runCheck})
}

// searcher is the part of the registry index the gates use; *registry.Index satisfies it.
type searcher interface {
	Search(query string, limit int) []registry.Result
}

// loadIndex loads the registry without project discovery. Tests replace it.
var loadIndex = func(getenv func(string) string) (searcher, error) {
	ix, err := registry.Load(getenv, registry.Options{NoProject: true})
	if err != nil {
		return nil, err
	}
	return ix, nil
}

// registrySource is Result.Source for agent-dry's own tools and skills, which live in the
// repository; other sources (skills installed in a home directory) differ per host.
const registrySource = "agent-dry"

// duplicates returns hits at or above the threshold that name an existing entry, skipping
// wanted families and earlier misses (both mean "build this"), hits named self and hits whose
// name is in covered (declared through extends or distinct_from). With registryOnly it keeps
// only agent-dry's own entries, so the admission gate gives the same answer on every host.
func duplicates(ix searcher, query, self string, covered map[string]bool, registryOnly bool) []registry.Result {
	var out []registry.Result
	for _, h := range ix.Search(query, 10) {
		if h.Score < registry.Threshold || h.Kind == "wanted" || h.Kind == "seen" {
			continue
		}
		if registryOnly && h.Source != registrySource {
			continue
		}
		if (self != "" && h.Name == self) || covered[h.Name] {
			continue
		}
		out = append(out, h)
	}
	return out
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// parseInterleaved parses flags that may appear before or after positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// Glyphs the repository rejects, built from bytes so this source never contains them.
var (
	emDash = string([]byte{0xe2, 0x80, 0x94})
	enDash = string([]byte{0xe2, 0x80, 0x93})
)

func hasDash(s string) bool { return strings.Contains(s, emDash) || strings.Contains(s, enDash) }

// capBuffer keeps at most max bytes: the first ones, or the last ones when tail is set.
type capBuffer struct {
	max  int
	tail bool
	buf  []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.tail {
		b.buf = append(b.buf, p...)
		if over := len(b.buf) - b.max; over > 0 {
			b.buf = append([]byte(nil), b.buf[over:]...)
		}
		return len(p), nil
	}
	if room := b.max - len(b.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *capBuffer) String() string { return string(b.buf) }

// proc is the result of one child process.
type proc struct {
	code     int
	stdout   string // first 64 KiB; with combined output, the last 16 KiB of both streams
	stderr   string // last 16 KiB
	timedOut bool
	startErr error
}

// childEnv is the process environment without GOFLAGS, matching the repository rule that Go
// runs as `env -u GOFLAGS go`.
func childEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GOFLAGS=") {
			out = append(out, kv)
		}
	}
	return out
}

// runProc runs name with args in dir under a timeout, with stdin from /dev/null. The child runs
// in its own process group, which is killed on timeout and after exit.
func runProc(dir string, timeout time.Duration, combined bool, name string, args ...string) proc {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = childEnv()
	cmd.WaitDelay = 2 * time.Second
	setProcGroup(cmd)
	out := &capBuffer{max: 64 << 10}
	errBuf := &capBuffer{max: 16 << 10, tail: true}
	if combined {
		out = errBuf
	}
	cmd.Stdout, cmd.Stderr = out, errBuf
	if err := cmd.Start(); err != nil {
		return proc{code: -1, startErr: err}
	}
	err := cmd.Wait()
	killProcGroup(cmd)
	p := proc{stdout: out.String(), stderr: errBuf.String()}
	if ctx.Err() == context.DeadlineExceeded {
		p.timedOut = true
		p.code = -1
		return p
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		p.code = cmd.ProcessState.ExitCode()
	case errors.As(err, &exitErr):
		p.code = exitErr.ExitCode()
	default:
		p.code = -1
		p.startErr = err
	}
	return p
}

// lastLine is the last non-empty line of s, trimmed to 160 bytes.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n\r\t "), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 160 {
		l = l[:157] + "..."
	}
	return l
}

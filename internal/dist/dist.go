// Package dist installs agent-dry onto a host, vendors tools into repositories with drift checks,
// builds release bundles and installs managed instruction sections.
// It registers the install, doctor, vendor, bundle and section commands.
//
// Interface used by other packages (keep signatures stable):
//
//	CheckVendored(root, repo string) (*VendorReport, error)
package dist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moabualruz/agent-dry/internal/cli"
)

func init() {
	cli.Register(cli.Command{Name: "install", Summary: "link agent-dry and global tools onto PATH, here or on a configured host", Run: runInstall})
	cli.Register(cli.Command{Name: "doctor", Summary: "report config, root, PATH, install links, index freshness and toolchain", Run: runDoctor})
	cli.Register(cli.Command{Name: "vendor", Summary: "copy a tool into a repository with checksums, or check copies for drift", Run: runVendor})
	cli.Register(cli.Command{Name: "bundle", Summary: "cross-compile agent-dry and pack a release tarball", Run: runBundle})
	cli.Register(cli.Command{Name: "section", Summary: "install or check a managed section in every harness instruction file", Run: runSection})
}

// The dash glyphs are built from byte escapes so no source file contains them.
const (
	emDash = "\xe2\x80\x94"
	enDash = "\xe2\x80\x93"
)

func hasDash(s string) bool { return strings.Contains(s, emDash) || strings.Contains(s, enDash) }

// parseArgs parses flags that may appear anywhere among positional arguments. Everything after a
// bare "--" is positional. ok is false when the caller should return code (help or usage error).
func parseArgs(env *cli.Env, fs *flag.FlagSet, usage string) (pos []string, code int, ok bool) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	args := env.Args
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(env.Stdout, usage)
				return nil, cli.ExitOK, false
			}
			return nil, cli.Usagef(env, "%s: %v; run agent-dry %s --help", fs.Name(), err, fs.Name()), false
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return append(pos, tail...), cli.ExitOK, true
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeFileAtomic writes data to a temporary file in the same directory, applies mode and renames
// it over path, so readers see the old or the new content and never a partial file.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".agent-dry-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(mode.Perm()); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}

func removeFileSync(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}

// writeJSON prints v as indented JSON.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func marshalJSON(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeJSON(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// within reports whether path lies strictly inside dir. Both must be absolute and clean.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// lookPath finds an executable regular file named name in pathEnv.
func lookPath(name, pathEnv string) (string, bool) {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// absPath makes p absolute and clean, expanding a leading ~/ with home.
func absPath(p, home string) (string, error) {
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	return filepath.Abs(p)
}

// realOrSelf resolves symlinks, returning the cleaned input when resolution fails.
func realOrSelf(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

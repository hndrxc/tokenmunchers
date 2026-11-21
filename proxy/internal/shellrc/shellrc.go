// Package shellrc manages the munch block inside a user's shell profile.
//
// The block is delimited by marker lines so it can be replaced in place on
// reinstall and removed cleanly on uninstall. Everything outside the block is
// left byte-identical.
package shellrc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Shell identifies a supported shell.
type Shell string

const (
	Bash Shell = "bash"
	Zsh  Shell = "zsh"
	Fish Shell = "fish"
)

// Profile is a shell and the absolute path of the rc file munch manages.
type Profile struct {
	Shell Shell
	Path  string
}

const (
	startMarker = "# >>> munch >>>"
	endMarker   = "# <<< munch <<<"
	noticeLine  = "# Managed by munch install; removed by munch uninstall."
)

// Detect maps env("SHELL") to a profile. ok is false for unsupported shells.
func Detect(env func(string) string, home, goos string) (Profile, bool) {
	switch Shell(filepath.Base(env("SHELL"))) {
	case Bash:
		return bashProfile(home, goos), true
	case Zsh:
		return zshProfile(env, home), true
	case Fish:
		return fishProfile(env, home), true
	}
	return Profile{}, false
}

// Candidates returns every profile munch may have written, for uninstall.
func Candidates(env func(string) string, home, goos string) []Profile {
	return []Profile{
		bashProfile(home, goos),
		zshProfile(env, home),
		fishProfile(env, home),
	}
}

func bashProfile(home, goos string) Profile {
	name := ".bashrc"
	if goos == "darwin" {
		name = ".bash_profile"
	}
	return Profile{Shell: Bash, Path: filepath.Join(home, name)}
}

func zshProfile(env func(string) string, home string) Profile {
	dir := env("ZDOTDIR")
	if dir == "" {
		dir = home
	}
	return Profile{Shell: Zsh, Path: filepath.Join(dir, ".zshrc")}
}

func fishProfile(env func(string) string, home string) Profile {
	dir := env("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".config")
	}
	return Profile{Shell: Fish, Path: filepath.Join(dir, "fish", "conf.d", "munch.fish")}
}

// State describes what a profile already contains.
type State struct {
	// Managed: the file contains the munch managed block.
	Managed bool
	// Unmanaged: a non-comment line outside the block invokes `munch env`.
	Unmanaged bool
}

var invokesEnv = regexp.MustCompile(`munch['"]?\s+env\b`)

// Inspect reports the state of p. A missing file yields the zero State.
func Inspect(p Profile) (State, error) {
	content, _, err := readTarget(p.Path)
	if err != nil {
		return State{}, err
	}
	if content == "" {
		return State{}, nil
	}
	start, end, found, err := locate(content)
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", p.Path, err)
	}
	outside := content
	if found {
		outside = content[:start] + content[end:]
	}
	st := State{Managed: found}
	for _, line := range strings.Split(outside, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if invokesEnv.MatchString(t) {
			st.Unmanaged = true
			break
		}
	}
	return st, nil
}

// Install writes the managed block invoking binPath into p.Path, replacing an
// existing block in place or appending one. changed is false when the
// identical block is already present.
func Install(p Profile, binPath string) (changed bool, err error) {
	block, err := render(p.Shell, binPath)
	if err != nil {
		return false, err
	}
	content, mode, err := readTarget(p.Path)
	if err != nil {
		return false, err
	}
	start, end, found, err := locate(content)
	if err != nil {
		return false, fmt.Errorf("%s: %w", p.Path, err)
	}
	var next string
	if found {
		if content[start:end] == block {
			return false, nil
		}
		next = content[:start] + block + content[end:]
	} else {
		next = appendBlock(content, block)
	}
	if err := writeTarget(p.Path, next, mode); err != nil {
		return false, err
	}
	return true, nil
}

// Remove deletes the managed block (and the blank separator Install added
// before it). A missing file or block yields false, nil. A fish conf.d file
// left with only whitespace is deleted.
func Remove(p Profile) (changed bool, err error) {
	content, mode, err := readTarget(p.Path)
	if err != nil {
		return false, err
	}
	start, end, found, err := locate(content)
	if err != nil {
		return false, fmt.Errorf("%s: %w", p.Path, err)
	}
	if !found {
		return false, nil
	}
	if strings.HasSuffix(content[:start], "\n\n") {
		start-- // the blank separator line Install added
	}
	next := content[:start] + content[end:]
	if p.Shell == Fish && strings.TrimSpace(next) == "" {
		if err := os.Remove(p.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return true, nil
	}
	if err := writeTarget(p.Path, next, mode); err != nil {
		return false, err
	}
	return true, nil
}

func appendBlock(content, block string) string {
	if content == "" {
		return block
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + "\n" + block
}

func render(sh Shell, binPath string) (string, error) {
	var b strings.Builder
	b.WriteString(startMarker + "\n" + noticeLine + "\n")
	switch sh {
	case Bash, Zsh:
		q := posixQuote(binPath)
		fmt.Fprintf(&b, "if [ -x %s ]; then\n  eval \"$(%s env --check --shell sh)\"\nfi\n", q, q)
	case Fish:
		q := fishQuote(binPath)
		fmt.Fprintf(&b, "if test -x %s\n    %s env --check --shell fish | source\nend\n", q, q)
	default:
		return "", fmt.Errorf("shellrc: unsupported shell %q", sh)
	}
	b.WriteString(endMarker + "\n")
	return b.String(), nil
}

func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fishQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// locate finds the managed block. start is the offset of the start marker
// line; end is the offset just past the end marker line (including its
// newline when present). Mismatched, reversed, or duplicate markers error.
func locate(content string) (start, end int, found bool, err error) {
	var starts, ends []int // line offsets
	var endsAfter []int    // offsets just past each end-marker line
	off := 0
	for off < len(content) {
		nl := strings.IndexByte(content[off:], '\n')
		next := len(content)
		line := content[off:]
		if nl >= 0 {
			next = off + nl + 1
			line = content[off : off+nl]
		}
		switch strings.TrimRight(line, "\r") {
		case startMarker:
			starts = append(starts, off)
		case endMarker:
			ends = append(ends, off)
			endsAfter = append(endsAfter, next)
		}
		off = next
	}
	switch {
	case len(starts) == 0 && len(ends) == 0:
		return 0, 0, false, nil
	case len(starts) != 1 || len(ends) != 1:
		return 0, 0, false, fmt.Errorf("malformed munch block: %d start marker(s), %d end marker(s); fix or remove it by hand", len(starts), len(ends))
	case ends[0] < starts[0]:
		return 0, 0, false, errors.New("malformed munch block: end marker precedes start marker; fix or remove it by hand")
	}
	return starts[0], endsAfter[0], true, nil
}

// readTarget reads the file behind path (following symlinks). A missing file
// yields empty content and the default mode.
func readTarget(path string) (string, os.FileMode, error) {
	target, err := resolve(path)
	if err != nil {
		return "", 0, err
	}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0o644, nil
	}
	if err != nil {
		return "", 0, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", 0, err
	}
	return string(data), info.Mode().Perm(), nil
}

// resolve follows symlinks when path exists so writes update the link target.
func resolve(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// writeTarget atomically writes content to the file behind path.
func writeTarget(path, content string, mode os.FileMode) error {
	target, err := resolve(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".munch-rc-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return err
	}
	return nil
}

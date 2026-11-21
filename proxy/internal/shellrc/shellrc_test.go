package shellrc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDetect(t *testing.T) {
	home := "/home/u"
	cases := []struct {
		name string
		env  map[string]string
		goos string
		want Profile
		ok   bool
	}{
		{"bash linux", map[string]string{"SHELL": "/bin/bash"}, "linux", Profile{Bash, "/home/u/.bashrc"}, true},
		{"bash darwin", map[string]string{"SHELL": "/opt/homebrew/bin/bash"}, "darwin", Profile{Bash, "/home/u/.bash_profile"}, true},
		{"zsh default", map[string]string{"SHELL": "/bin/zsh"}, "linux", Profile{Zsh, "/home/u/.zshrc"}, true},
		{"zsh zdotdir", map[string]string{"SHELL": "/usr/bin/zsh", "ZDOTDIR": "/home/u/.config/zsh"}, "linux", Profile{Zsh, "/home/u/.config/zsh/.zshrc"}, true},
		{"fish default", map[string]string{"SHELL": "/usr/bin/fish"}, "linux", Profile{Fish, "/home/u/.config/fish/conf.d/munch.fish"}, true},
		{"fish xdg", map[string]string{"SHELL": "/usr/bin/fish", "XDG_CONFIG_HOME": "/x/cfg"}, "linux", Profile{Fish, "/x/cfg/fish/conf.d/munch.fish"}, true},
		{"sh", map[string]string{"SHELL": "/bin/sh"}, "linux", Profile{}, false},
		{"dash", map[string]string{"SHELL": "/bin/dash"}, "linux", Profile{}, false},
		{"pwsh", map[string]string{"SHELL": "/usr/bin/pwsh"}, "linux", Profile{}, false},
		{"empty", map[string]string{}, "linux", Profile{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Detect(mapEnv(c.env), home, c.goos)
			if ok != c.ok || got != c.want {
				t.Fatalf("Detect = %+v, %v; want %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestCandidates(t *testing.T) {
	got := Candidates(mapEnv(map[string]string{"ZDOTDIR": "/z"}), "/home/u", "linux")
	want := []Profile{
		{Bash, "/home/u/.bashrc"},
		{Zsh, "/z/.zshrc"},
		{Fish, "/home/u/.config/fish/conf.d/munch.fish"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestInstallAppendAndRemoveRestores(t *testing.T) {
	p := Profile{Bash, filepath.Join(t.TempDir(), ".bashrc")}
	orig := "export FOO=1\nalias ll='ls -l'\n"
	write(t, p.Path, orig)

	changed, err := Install(p, "/usr/local/bin/munch")
	if err != nil || !changed {
		t.Fatalf("Install = %v, %v", changed, err)
	}
	got := read(t, p.Path)
	if !strings.HasPrefix(got, orig+"\n"+startMarker+"\n") || !strings.HasSuffix(got, endMarker+"\n") {
		t.Fatalf("unexpected content:\n%s", got)
	}
	if !strings.Contains(got, "eval \"$('/usr/local/bin/munch' env --check --shell sh)\"") {
		t.Fatalf("missing eval line:\n%s", got)
	}
	if info, _ := os.Stat(p.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want preserved 0600", info.Mode().Perm())
	}

	changed, err = Remove(p)
	if err != nil || !changed {
		t.Fatalf("Remove = %v, %v", changed, err)
	}
	if got := read(t, p.Path); got != orig {
		t.Fatalf("after Remove = %q, want %q", got, orig)
	}
	changed, err = Remove(p)
	if err != nil || changed {
		t.Fatalf("second Remove = %v, %v", changed, err)
	}
}

func TestInstallIdempotent(t *testing.T) {
	p := Profile{Zsh, filepath.Join(t.TempDir(), ".zshrc")}
	if changed, err := Install(p, "/bin/munch"); err != nil || !changed {
		t.Fatalf("first Install = %v, %v", changed, err)
	}
	first := read(t, p.Path)
	if info, _ := os.Stat(p.Path); info.Mode().Perm() != 0o644 {
		t.Fatalf("new file mode = %v, want 0644", info.Mode().Perm())
	}
	if changed, err := Install(p, "/bin/munch"); err != nil || changed {
		t.Fatalf("second Install = %v, %v", changed, err)
	}
	if read(t, p.Path) != first {
		t.Fatal("content changed on idempotent install")
	}
}

func TestInstallAppendsNewlineWhenMissing(t *testing.T) {
	p := Profile{Bash, filepath.Join(t.TempDir(), ".bashrc")}
	write(t, p.Path, "export A=1")
	if _, err := Install(p, "/bin/munch"); err != nil {
		t.Fatal(err)
	}
	got := read(t, p.Path)
	if !strings.HasPrefix(got, "export A=1\n\n"+startMarker+"\n") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("unexpected content:\n%q", got)
	}
}

func TestInstallReplacesInPlace(t *testing.T) {
	p := Profile{Bash, filepath.Join(t.TempDir(), ".bashrc")}
	write(t, p.Path, "before\n")
	if _, err := Install(p, "/old/munch"); err != nil {
		t.Fatal(err)
	}
	// Add content after the block.
	f, err := os.OpenFile(p.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	changed, err := Install(p, "/new/munch")
	if err != nil || !changed {
		t.Fatalf("Install = %v, %v", changed, err)
	}
	got := read(t, p.Path)
	if strings.Contains(got, "/old/munch") || strings.Count(got, startMarker) != 1 {
		t.Fatalf("block not replaced:\n%s", got)
	}
	if !strings.HasPrefix(got, "before\n\n"+startMarker) || !strings.HasSuffix(got, endMarker+"\nafter\n") {
		t.Fatalf("surrounding content altered:\n%s", got)
	}
	if !strings.Contains(got, "'/new/munch'") {
		t.Fatalf("new path missing:\n%s", got)
	}
}

func TestMalformedMarkersError(t *testing.T) {
	cases := map[string]string{
		"start only": "a\n" + startMarker + "\nb\n",
		"end only":   "a\n" + endMarker + "\nb\n",
		"reversed":   endMarker + "\n" + startMarker + "\n",
		"duplicate":  startMarker + "\n" + endMarker + "\n" + startMarker + "\n" + endMarker + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := Profile{Bash, filepath.Join(t.TempDir(), ".bashrc")}
			write(t, p.Path, content)
			if _, err := Install(p, "/bin/munch"); err == nil {
				t.Fatal("Install: expected error")
			}
			if _, err := Remove(p); err == nil {
				t.Fatal("Remove: expected error")
			}
			if _, err := Inspect(p); err == nil {
				t.Fatal("Inspect: expected error")
			}
			if got := read(t, p.Path); got != content {
				t.Fatalf("file modified: %q", got)
			}
		})
	}
}

func TestSymlinkedRCStaysSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "bashrc")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, target, "export X=1\n")
	link := filepath.Join(dir, ".bashrc")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	p := Profile{Bash, link}

	if _, err := Install(p, "/bin/munch"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link no longer a symlink: %v %v", info, err)
	}
	if !strings.Contains(read(t, target), startMarker) {
		t.Fatal("target not updated")
	}
	if _, err := Remove(p); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link no longer a symlink after Remove: %v %v", info, err)
	}
	if got := read(t, target); got != "export X=1\n" {
		t.Fatalf("target after Remove = %q", got)
	}
}

func TestInspect(t *testing.T) {
	dir := t.TempDir()
	p := Profile{Bash, filepath.Join(dir, ".bashrc")}

	if st, err := Inspect(p); err != nil || st != (State{}) {
		t.Fatalf("missing file: %+v, %v", st, err)
	}

	write(t, p.Path, "# eval \"$(munch env --check)\"\n  # munch env\nexport A=1\n")
	if st, _ := Inspect(p); st != (State{}) {
		t.Fatalf("commented line flagged: %+v", st)
	}

	// Block only: path ends in munch, so the block itself matches the regexp.
	write(t, p.Path, "")
	if _, err := Install(p, "/opt/munch"); err != nil {
		t.Fatal(err)
	}
	if st, _ := Inspect(p); st != (State{Managed: true}) {
		t.Fatalf("block only: %+v", st)
	}

	// Hand-written line outside the block.
	write(t, p.Path, read(t, p.Path)+"eval \"$(munch env --check)\"\n")
	if st, _ := Inspect(p); st != (State{Managed: true, Unmanaged: true}) {
		t.Fatalf("hand-written: %+v", st)
	}

	write(t, p.Path, "eval \"$('/home/u/bin/munch' env --check)\"\n")
	if st, _ := Inspect(p); st != (State{Unmanaged: true}) {
		t.Fatalf("quoted hand-written: %+v", st)
	}
}

func TestFishInstallAndRemoveDeletesFile(t *testing.T) {
	p := Profile{Fish, filepath.Join(t.TempDir(), "fish", "conf.d", "munch.fish")}
	if changed, err := Install(p, `/we'ird\bin/munch`); err != nil || !changed {
		t.Fatalf("Install = %v, %v", changed, err)
	}
	got := read(t, p.Path)
	want := "if test -x '/we\\'ird\\\\bin/munch'\n    '/we\\'ird\\\\bin/munch' env --check --shell fish | source\nend\n"
	if !strings.Contains(got, want) {
		t.Fatalf("fish body wrong:\n%s", got)
	}
	if changed, err := Remove(p); err != nil || !changed {
		t.Fatalf("Remove = %v, %v", changed, err)
	}
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted, stat err = %v", err)
	}
}

func TestUnsupportedShellInstall(t *testing.T) {
	p := Profile{Shell: "pwsh", Path: filepath.Join(t.TempDir(), "x")}
	if _, err := Install(p, "/bin/munch"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBashBlockSourcedInRealShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, name := range []string{"munch", "it's a dir"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "munch")
			script := "#!/bin/sh\n[ \"$1\" = env ] && [ \"$2\" = --check ] && [ \"$3\" = --shell ] && [ \"$4\" = sh ] || exit 1\necho 'export TM_PROBE=ok'\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			rc := Profile{Bash, filepath.Join(t.TempDir(), ".bashrc")}
			if _, err := Install(rc, bin); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bash, "-c", `. "$1"; printf %s "$TM_PROBE"`, "bash", rc.Path).CombinedOutput()
			if err != nil {
				t.Fatalf("bash: %v\n%s", err, out)
			}
			if string(out) != "ok" {
				t.Fatalf("TM_PROBE = %q, want ok", out)
			}
		})
	}
}

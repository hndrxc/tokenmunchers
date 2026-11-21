package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/service"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/shellrc"
)

func TestCLILoginEnvRoutesPauseAndStats(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	stateDir := filepath.Join(tmp, "state")

	run := func(args ...string) (int, string, string) {
		var out, errBuf bytes.Buffer
		app := &App{
			Stdin:  strings.NewReader(""),
			Stdout: &out,
			Stderr: &errBuf,
		}
		code := app.Run(args)
		return code, out.String(), errBuf.String()
	}

	// Login with invalid key fails.
	if code, _, _ := run("login", "--config-dir", cfgDir, "--state-dir", stateDir, "bad_key"); code == 0 {
		t.Fatal("expected non-zero exit for invalid key")
	}

	// Login with valid key succeeds.
	validKey := "tm_" + strings.Repeat("K", 43)
	if code, out, errOut := run("login", "--config-dir", cfgDir, "--state-dir", stateDir, validKey); code != 0 {
		t.Fatalf("login failed (code=%d): out=%s err=%s", code, out, errOut)
	}

	// Env output (unchecked) uses the private capability URL; it is created on demand.
	code, out, _ := run("env", "--check=false", "--config-dir", cfgDir, "--state-dir", stateDir, "--shell", "bash")
	key, err := localauth.Load(cfgDir)
	if err != nil {
		t.Fatalf("expected env to create local key: %v", err)
	}
	prefix := key.RoutePrefix()
	if code != 0 || !strings.Contains(out, `export ANTHROPIC_BASE_URL="http://127.0.0.1:8787`+prefix+`/anthropic"`) {
		t.Fatalf("unexpected bash env output (code=%d)", code)
	}
	code, out, _ = run("env", "--check=false", "--config-dir", cfgDir, "--state-dir", stateDir, "--shell", "fish")
	if code != 0 || !strings.Contains(out, `set -gx OPENAI_BASE_URL "http://127.0.0.1:8787`+prefix+`/openai/v1"`) {
		t.Fatalf("unexpected fish env output (code=%d)", code)
	}
	code, out, _ = run("env", "--config-dir", cfgDir, "--state-dir", stateDir, "--shell", "bash", "--unset")
	if code != 0 || !strings.Contains(out, "unset ANTHROPIC_BASE_URL") || strings.Contains(out, prefix) {
		t.Fatalf("unexpected unset env output (code=%d)", code)
	}

	// The default prints nothing when no proxy proves readiness.
	code, out, _ = run("env", "--config-dir", cfgDir, "--state-dir", stateDir, "--listen", "127.0.0.1:1")
	if code != 0 || out != "" {
		t.Fatalf("expected empty env output with no proxy (code=%d): %q", code, out)
	}

	// Exec refuses to launch the child when no proxy is ready.
	if code, out, _ := run("exec", "--config-dir", cfgDir, "--state-dir", stateDir, "--listen", "127.0.0.1:1", "--", "sh", "-c", "echo ran"); code == 0 || strings.Contains(out, "ran") {
		t.Fatalf("exec must not run child without a ready proxy (code=%d, out=%q)", code, out)
	}

	// Routes listing includes core providers and redacts the capability by default.
	code, out, _ = run("routes", "--config-dir", cfgDir, "--state-dir", stateDir)
	if code != 0 || !strings.Contains(out, "anthropic") || !strings.Contains(out, "openai") || !strings.Contains(out, "gemini") {
		t.Fatalf("unexpected routes output: %s", out)
	}
	if strings.Contains(out, prefix) || !strings.Contains(out, "<redacted>") {
		t.Fatalf("routes must redact the capability by default: %s", out)
	}
	code, out, _ = run("routes", "--config-dir", cfgDir, "--state-dir", stateDir, "--show-local-urls")
	if code != 0 || !strings.Contains(out, "127.0.0.1:8787"+prefix+"/anthropic") {
		t.Fatalf("expected --show-local-urls to print capability URLs: %s", out)
	}

	// Pause & resume persist offline cleanly.
	if code, _, errOut := run("pause", "--config-dir", cfgDir, "--state-dir", stateDir, "--listen", "127.0.0.1:1"); code != 0 {
		t.Fatalf("pause failed: %s", errOut)
	}
	if code, _, errOut := run("resume", "--config-dir", cfgDir, "--state-dir", stateDir, "--listen", "127.0.0.1:1"); code != 0 {
		t.Fatalf("resume failed: %s", errOut)
	}

	// Empty stats succeeds cleanly.
	if code, out, errOut := run("stats", "--config-dir", cfgDir, "--state-dir", stateDir); code != 0 || !strings.Contains(out, "Calls:") {
		t.Fatalf("stats failed (code=%d): out=%s err=%s", code, out, errOut)
	}
}

// runApp runs one CLI invocation with the given stdin.
func runApp(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	app := &App{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errBuf}
	code = app.Run(args)
	return code, out.String(), errBuf.String()
}

// envNames lists the variables `munch env` exports.
func envNames() []string {
	var names []string
	for _, kv := range EnvPairs("") {
		names = append(names, kv[0])
	}
	return names
}

// writeEnvConfig creates a private config directory holding config.json and
// returns it with a state directory path.
func writeEnvConfig(t *testing.T, content string) (cfgDir, stateDir string) {
	t.Helper()
	tmp := t.TempDir()
	cfgDir = filepath.Join(tmp, "cfg")
	stateDir = filepath.Join(tmp, "state")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgDir, stateDir
}

func TestEnvOMPWrapperOutput(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no omp, so only omp_via_plugin adds the wrapper
	names := envNames()
	cases := []struct {
		shell    string
		export   string // start of the first export line
		define   string // start of the omp wrapper
		undefine string // line that removes the wrapper
	}{
		{"sh", "export ANTHROPIC_BASE_URL=", "omp() { command env -u ANTHROPIC_BASE_URL -u OPENAI_BASE_URL ", "unset -f omp\n"},
		{"fish", "set -gx ANTHROPIC_BASE_URL ", "function omp; command env -u ANTHROPIC_BASE_URL -u OPENAI_BASE_URL ", "functions -e omp\n"},
		{"powershell", "$env:ANTHROPIC_BASE_URL = ", "function omp {", "Remove-Item Function:omp -ErrorAction SilentlyContinue\n"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			onCfg, onState := writeEnvConfig(t, `{"omp_via_plugin": true}`)
			offCfg, offState := writeEnvConfig(t, `{}`)

			code, on, errOut := runApp("", "env", "--check=false", "--shell", tc.shell, "--config-dir", onCfg, "--state-dir", onState)
			if code != 0 {
				t.Fatalf("env failed (code=%d): %s", code, errOut)
			}
			exportAt, defineAt := strings.Index(on, tc.export), strings.Index(on, tc.define)
			if exportAt < 0 || defineAt < 0 || exportAt > defineAt {
				t.Fatalf("expected the exports followed by the omp wrapper:\n%s", on)
			}
			for _, name := range names {
				var want string
				if tc.shell == "powershell" {
					want = "'" + name + "'"
				} else {
					want = " -u " + name + " "
				}
				if n := strings.Count(on, want); n != 1 {
					t.Errorf("wrapper mentions %s %d times, want once:\n%s", name, n, on)
				}
			}
			if tc.shell == "powershell" && !strings.Contains(on, "Get-Command omp -CommandType Application") {
				t.Errorf("PowerShell wrapper must run the omp application, not itself:\n%s", on)
			}

			// Without the flag the output is just the exports.
			code, off, errOut := runApp("", "env", "--check=false", "--shell", tc.shell, "--config-dir", offCfg, "--state-dir", offState)
			if code != 0 {
				t.Fatalf("env failed (code=%d): %s", code, errOut)
			}
			if strings.Contains(off, "omp") || strings.Count(off, "\n") != len(names) {
				t.Errorf("omp_via_plugin=false must print only the %d exports:\n%s", len(names), off)
			}

			// --unset removes the wrapper only when the flag is on.
			_, unsetOn, _ := runApp("", "env", "--unset", "--shell", tc.shell, "--config-dir", onCfg, "--state-dir", onState)
			if !strings.Contains(unsetOn, tc.undefine) || strings.Contains(unsetOn, tc.define) {
				t.Errorf("--unset with omp_via_plugin must remove the wrapper:\n%s", unsetOn)
			}
			_, unsetOff, _ := runApp("", "env", "--unset", "--shell", tc.shell, "--config-dir", offCfg, "--state-dir", offState)
			if strings.Contains(unsetOff, tc.undefine) {
				t.Errorf("--unset without omp_via_plugin must not touch omp:\n%s", unsetOff)
			}

			// An unverified daemon still prints nothing, wrapper included.
			code, quiet, _ := runApp("", "env", "--shell", tc.shell, "--listen", "127.0.0.1:1", "--config-dir", onCfg, "--state-dir", onState)
			if code != 0 || quiet != "" {
				t.Errorf("env --check must print nothing without a verified proxy (code=%d): %q", code, quiet)
			}
		})
	}
}

// fakeOMPScript and fakeOtherScript stand in for the omp binary and another AI
// tool. Both print their environment, so a test can see what a child inherits.
const fakeOMPScript = `#!/bin/sh
echo OMP_RAN
printf 'ARG=%s\n' "$@"
env
`

const fakeOtherScript = `#!/bin/sh
echo OTHER_RAN
env
`

// origPATH is the PATH the tests started with; the PATH the process sees is
// replaced per test, but a real shell session still needs the system tools.
var origPATH = os.Getenv("PATH")

// shellRun is what one real shell session observed.
type shellRun struct {
	envOutput  string // what `munch env` printed
	omp        string // output of the fake omp, including its environment
	other      string // output of the sibling command, including its environment
	parentKept bool   // the shell still had the proxy variables after running omp
}

// runShellSession evals the output of `munch env --shell syntax` in a real
// shell, then runs the fake omp and a sibling command found first on PATH.
// ompVisible says whether `munch env` itself can find omp on its PATH; the
// shell always can.
func runShellSession(t *testing.T, shellPath, syntax string, argv func(envFile string) []string, viaPlugin, ompVisible bool) shellRun {
	t.Helper()
	cfg := `{}`
	if viaPlugin {
		cfg = `{"omp_via_plugin": true}`
	}
	cfgDir, stateDir := writeEnvConfig(t, cfg)
	tmp := filepath.Dir(cfgDir)
	shellPATH := origPATH

	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"omp": fakeOMPScript, "othertool": fakeOtherScript} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ompVisible {
		t.Setenv("PATH", binDir)
	} else {
		t.Setenv("PATH", filepath.Join(tmp, "no-such-bin"))
	}

	code, envOut, errOut := runApp("", "env", "--check=false", "--shell", syntax, "--config-dir", cfgDir, "--state-dir", stateDir)
	if code != 0 {
		t.Fatalf("munch env failed (code=%d): %s", code, errOut)
	}
	envFile := filepath.Join(tmp, "munch-env")
	if err := os.WriteFile(envFile, []byte(envOut), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(shellPath, argv(envFile)...)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + shellPATH,
		"HOME=" + tmp,
		"XDG_CONFIG_HOME=" + filepath.Join(tmp, "xdg"),
	}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(shellPath), err, raw)
	}
	ompPart, rest, ok := strings.Cut(string(raw), "AFTER=")
	if !ok {
		t.Fatalf("no AFTER marker in shell output:\n%s", raw)
	}
	after, other, ok := strings.Cut(rest, "OTHER_RAN")
	if !ok {
		t.Fatalf("no OTHER_RAN marker in shell output:\n%s", raw)
	}
	return shellRun{envOutput: envOut, omp: ompPart, other: other, parentKept: strings.HasPrefix(after, "set")}
}

func posixShellArgs(envFile string) []string {
	return []string{"-c", `eval "$(cat "$0")"
omp one "two words"
echo "AFTER=${ANTHROPIC_BASE_URL:+set}"
othertool
`, envFile}
}

func fishShellArgs(envFile string) []string {
	return []string{"-c", "source '" + envFile + `'
omp one 'two words'
if test -n "$ANTHROPIC_BASE_URL"
    echo AFTER=set
else
    echo AFTER=
end
othertool
`}
}

func TestEnvOMPWrapperRealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	shells := []struct {
		name   string // shell binary to run
		syntax string // value for munch env --shell
		define string // text that defines the omp wrapper in that syntax
		argv   func(envFile string) []string
	}{
		{"bash", "sh", "omp()", posixShellArgs},
		{"zsh", "sh", "omp()", posixShellArgs},
		{"fish", "fish", "function omp", fishShellArgs},
	}
	for _, sh := range shells {
		t.Run(sh.name, func(t *testing.T) {
			path, err := exec.LookPath(sh.name)
			if err != nil {
				t.Skipf("%s is not installed", sh.name)
			}
			names := envNames()

			// With omp_via_plugin the wrapper keeps every proxy variable away
			// from omp, while other commands and the shell itself keep them.
			via := runShellSession(t, path, sh.syntax, sh.argv, true, true)
			if !strings.Contains(via.envOutput, sh.define) {
				t.Fatalf("omp_via_plugin output has no omp wrapper:\n%s", via.envOutput)
			}
			if !strings.Contains(via.omp, "OMP_RAN") {
				t.Fatalf("the wrapper did not run the omp binary:\n%s", via.omp)
			}
			if !strings.Contains(via.omp, "ARG=one\n") || !strings.Contains(via.omp, "ARG=two words\n") {
				t.Errorf("the wrapper must pass arguments through unchanged:\n%s", via.omp)
			}
			for _, name := range names {
				if strings.Contains(via.omp, name+"=") {
					t.Errorf("%s reached omp through the wrapper", name)
				}
				if !strings.Contains(via.other, name+"=") {
					t.Errorf("%s is missing from a sibling command's environment", name)
				}
			}
			if !via.parentKept {
				t.Error("the wrapper must not unset the variables in the calling shell")
			}

			// Without omp_via_plugin but with omp on PATH, the wrapper hides only
			// OLLAMA_HOST from omp: OMP would keep just host:port of it.
			proxied := runShellSession(t, path, sh.syntax, sh.argv, false, true)
			if !strings.Contains(proxied.envOutput, sh.define) {
				t.Fatalf("omp on PATH should get a wrapper even without omp_via_plugin:\n%s", proxied.envOutput)
			}
			if !strings.Contains(proxied.omp, "OMP_RAN") || !strings.Contains(proxied.omp, "ARG=two words\n") {
				t.Fatalf("the wrapper did not run the omp binary with its arguments:\n%s", proxied.omp)
			}
			for _, name := range names {
				inOMP := strings.Contains(proxied.omp, name+"=")
				if name == "OLLAMA_HOST" && inOMP {
					t.Error("OLLAMA_HOST reached omp through the wrapper")
				}
				if name != "OLLAMA_HOST" && !inOMP {
					t.Errorf("omp should still see %s without omp_via_plugin", name)
				}
				if !strings.Contains(proxied.other, name+"=") {
					t.Errorf("%s is missing from a sibling command's environment", name)
				}
			}
			if !proxied.parentKept {
				t.Error("the wrapper must not unset the variables in the calling shell")
			}

			// Control: with neither the flag nor omp on munch's PATH there is no
			// wrapper and omp inherits everything, which proves the checks above
			// can fail.
			plain := runShellSession(t, path, sh.syntax, sh.argv, false, false)
			if strings.Contains(plain.envOutput, sh.define) {
				t.Errorf("no omp and omp_via_plugin=false output must not define omp:\n%s", plain.envOutput)
			}
			for _, name := range names {
				if !strings.Contains(plain.omp, name+"=") {
					t.Errorf("without the wrapper omp should inherit %s", name)
				}
			}
		})
	}
}

func TestEnvOMPWrapperWithoutPluginHidesOnlyOllamaHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs an executable named omp on PATH")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "omp"), []byte(fakeOMPScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	cases := []struct {
		shell    string
		define   string
		undefine string
	}{
		{"sh", "omp() { command env -u OLLAMA_HOST omp \"$@\"; }\n", "unset -f omp\n"},
		{"fish", "function omp; command env -u OLLAMA_HOST omp $argv; end\n", "functions -e omp\n"},
		{"powershell", "foreach ($name in 'OLLAMA_HOST') {", "Remove-Item Function:omp -ErrorAction SilentlyContinue\n"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			cfgDir, stateDir := writeEnvConfig(t, `{}`)
			code, out, errOut := runApp("", "env", "--check=false", "--shell", tc.shell, "--config-dir", cfgDir, "--state-dir", stateDir)
			if code != 0 {
				t.Fatalf("env failed (code=%d): %s", code, errOut)
			}
			if !strings.Contains(out, tc.define) {
				t.Errorf("omp on PATH should define a wrapper that hides only OLLAMA_HOST:\n%s", out)
			}
			if strings.Contains(out, " -u ANTHROPIC_BASE_URL") || strings.Contains(out, "'ANTHROPIC_BASE_URL'") {
				t.Errorf("without omp_via_plugin the wrapper must leave the other variables alone:\n%s", out)
			}
			_, unset, _ := runApp("", "env", "--unset", "--shell", tc.shell, "--config-dir", cfgDir, "--state-dir", stateDir)
			if !strings.Contains(unset, tc.undefine) {
				t.Errorf("--unset must remove the wrapper when omp is on PATH:\n%s", unset)
			}
		})
	}
}

func TestOMPStrippedVars(t *testing.T) {
	if got := ompStrippedVars(config.Config{}); !reflect.DeepEqual(got, []string{"OLLAMA_HOST"}) {
		t.Errorf("without omp_via_plugin only OLLAMA_HOST is stripped, got %v", got)
	}
	if got := ompStrippedVars(config.Config{OMPViaPlugin: true}); !reflect.DeepEqual(got, envNames()) {
		t.Errorf("omp_via_plugin strips every proxy variable, got %v", got)
	}
}

func TestEnvHookLine(t *testing.T) {
	cases := []struct {
		shell string
		flags []envFlag
		want  string
	}{
		{"sh", nil, `eval "$(munch env --check)"`},
		{"sh", []envFlag{{"--listen", "127.0.0.1:9000"}}, `eval "$(munch env --check --listen 127.0.0.1:9000)"`},
		{"sh", []envFlag{{"--config-dir", "/tmp/a b"}, {"--state-dir", "/tmp/it's"}}, `eval "$(munch env --check --config-dir '/tmp/a b' --state-dir '/tmp/it'\''s')"`},
		{"fish", nil, `munch env --check --shell fish | source`},
		{"fish", []envFlag{{"--config-dir", "/tmp/it's"}}, `munch env --check --shell fish --config-dir '/tmp/it\'s' | source`},
		{"powershell", nil, `munch env --shell powershell | Out-String | Invoke-Expression`},
		{"powershell", []envFlag{{"--config-dir", `C:\Users\me\my cfg`}, {"--listen", "127.0.0.1:9000"}}, `munch env --shell powershell --config-dir 'C:\Users\me\my cfg' --listen 127.0.0.1:9000 | Out-String | Invoke-Expression`},
	}
	for _, tc := range cases {
		if got := envHookLine(tc.shell, tc.flags); got != tc.want {
			t.Errorf("envHookLine(%q, %v)\n got: %s\nwant: %s", tc.shell, tc.flags, got, tc.want)
		}
	}
}

func TestQuoteArgRoundTripsThroughBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("bash is not available")
	}
	for _, v := range []string{"plain/path:8787", "with space", "it's", `say "hi"`, "$HOME and `cmd`", "a;b&c|d", "tab\there", "*glob?", "line\nbreak", `back\slash`} {
		out, err := exec.Command(bash, "-c", "printf %s "+quoteArg("sh", v)).Output()
		if err != nil {
			t.Errorf("bash rejected quoteArg(%q): %v", v, err)
			continue
		}
		if string(out) != v {
			t.Errorf("bash read quoteArg(%q) as %q", v, out)
		}
	}
}

func TestPrompterConfirm(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
		asked int // times the question is printed
	}{
		{"empty line is yes", "\n", true, 1},
		{"y", "y\n", true, 1},
		{"YES", "YES\n", true, 1},
		{"n", "n\n", false, 1},
		{"No", "No\n", false, 1},
		{"EOF is yes", "", true, 1},
		{"n without newline", "n", false, 1},
		{"unrecognized answer asks again", "maybe\nn\n", false, 2},
		{"unrecognized answer at EOF is yes", "maybe", true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(tc.input), &out, false)
			got, err := p.confirm("Proceed?")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("confirm on %q = %v, want %v", tc.input, got, tc.want)
			}
			if n := strings.Count(out.String(), "Proceed? [Y/n] "); n != tc.asked {
				t.Errorf("question printed %d times, want %d:\n%s", n, tc.asked, out.String())
			}
		})
	}
}

func TestPrompterYesNeverReadsInput(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("n\n"), &out, true)
	got, err := p.confirm("Proceed?")
	if err != nil || !got {
		t.Fatalf("confirm with --yes = %v, %v; want true", got, err)
	}
	if !strings.Contains(out.String(), "Proceed? [Y/n] y") {
		t.Errorf("--yes should show the answer it took: %q", out.String())
	}
	if line, _ := p.in.ReadString('\n'); line != "n\n" {
		t.Errorf("--yes consumed input: next line is %q", line)
	}
}

func TestPrompterAPIKey(t *testing.T) {
	valid := "tm_" + strings.Repeat("K", 43)
	read := func(input string) (string, error) {
		var out bytes.Buffer
		return newPrompter(strings.NewReader(input), &out, false).apiKey()
	}
	if got, err := read(valid + "\n"); err != nil || got != valid {
		t.Errorf("valid key: got %q, %v", got, err)
	}
	if got, err := read("  " + valid + "  \n"); err != nil || got != valid {
		t.Errorf("padded key: got %q, %v", got, err)
	}
	for _, empty := range []string{"", "\n", "   \n"} {
		if _, err := read(empty); !errors.Is(err, errNoKeyEntered) {
			t.Errorf("input %q: err = %v, want errNoKeyEntered", empty, err)
		}
	}
	if _, err := read("not-a-key\n"); err == nil || errors.Is(err, errNoKeyEntered) {
		t.Errorf("malformed key: err = %v, want a format error", err)
	}
}

func TestLoginPromptsAndPointsAtInstall(t *testing.T) {
	tmp := t.TempDir()
	cfgDir, stateDir := filepath.Join(tmp, "cfg"), filepath.Join(tmp, "state")
	key := "tm_" + strings.Repeat("K", 43)

	code, out, errOut := runApp(key+"\n", "login", "--config-dir", cfgDir, "--state-dir", stateDir)
	if code != 0 {
		t.Fatalf("login failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "Paste your TokenMunchers API key") || !strings.Contains(out, "`munch install`") {
		t.Errorf("login should prompt and point at install:\n%s", out)
	}
	if strings.Contains(out+errOut, key) {
		t.Error("login printed the API key")
	}

	if code, _, errOut := runApp("", "login", "--config-dir", cfgDir, "--state-dir", stateDir); code == 0 || !strings.Contains(errOut, "no API key entered") {
		t.Errorf("login with no input should say so (code=%d): %s", code, errOut)
	}
}

// fakeService records the calls install and uninstall make instead of touching
// the real service manager.
type fakeService struct {
	backend    string
	installErr error
	startErr   error
	running    bool
	calls      []string
}

var _ service.Manager = (*fakeService)(nil)

func (f *fakeService) Backend() string { return f.backend }

func (f *fakeService) Install(string) (string, error) {
	f.calls = append(f.calls, "install")
	return "/fake/tmproxy.service", f.installErr
}

func (f *fakeService) Uninstall() error {
	f.calls = append(f.calls, "uninstall")
	return nil
}

func (f *fakeService) Start(string) error {
	f.calls = append(f.calls, "start")
	if f.startErr == nil {
		f.running = true
	}
	return f.startErr
}

func (f *fakeService) Stop() error {
	f.calls = append(f.calls, "stop")
	f.running = false
	return nil
}

func (f *fakeService) Status() service.Info {
	return service.Info{Backend: f.backend, Running: f.running}
}

// installFixture isolates install and uninstall from the real machine: temp
// HOME, config and state directories, an empty PATH (so no omp), SHELL=bash,
// a fake service manager, and a stand-in daemon that answers the authenticated
// health check once install has created the local key.
type installFixture struct {
	t        *testing.T
	home     string
	cfgDir   string
	stateDir string
	listen   string
	key      string
	svc      *fakeService
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install tests use POSIX shell profiles")
	}
	tmp := t.TempDir()
	f := &installFixture{
		t:        t,
		home:     filepath.Join(tmp, "home"),
		cfgDir:   filepath.Join(tmp, "cfg"),
		stateDir: filepath.Join(tmp, "state"),
		key:      "tm_" + strings.Repeat("K", 43),
		svc:      &fakeService{backend: "systemd-user"},
	}
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localKey, err := localauth.Load(f.cfgDir)
		if err != nil || r.URL.Path != "/_tmproxy/health" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "version": "test", "pid": 4242,
			"proof": localKey.Proof(r.Header.Get("X-TMProxy-Challenge")),
		})
	}))
	t.Cleanup(srv.Close)
	f.listen = strings.TrimPrefix(srv.URL, "http://")

	t.Setenv("HOME", f.home)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", filepath.Join(tmp, "no-such-bin"))
	t.Setenv("TMPROXY_CONFIG_DIR", f.cfgDir)
	t.Setenv("TMPROXY_STATE_DIR", f.stateDir)
	t.Setenv("TMPROXY_LISTEN", f.listen)
	for _, name := range []string{
		"TOKENMUNCHERS_KEY", "TOKENMUNCHERS_API_KEY", "TOKENMUNCHERS_URL",
		"ZDOTDIR", "XDG_CONFIG_HOME", "CODEX_HOME",
		"PI_CONFIG_DIR", "PI_PROFILE", "OMP_PROFILE", "PI_CODING_AGENT_DIR",
	} {
		t.Setenv(name, "")
	}
	return f
}

// run executes a command with the fake service manager installed.
func (f *installFixture) run(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	app := &App{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errBuf}
	app.newService = func(config.Paths, string) service.Manager { return f.svc }
	code = app.Run(args)
	return code, out.String(), errBuf.String()
}

func (f *installFixture) paths() config.Paths {
	return config.Paths{ConfigDir: f.cfgDir, StateDir: f.stateDir}
}

// bashProfile is the profile install edits for SHELL=bash.
func (f *installFixture) bashProfile() shellrc.Profile {
	f.t.Helper()
	p, ok := shellrc.Detect(os.Getenv, f.home, runtime.GOOS)
	if !ok {
		f.t.Fatal("SHELL=/bin/bash should map to a profile")
	}
	return p
}

// writeConfig stores config.json in the fixture's private config directory.
func (f *installFixture) writeConfig(content string) {
	f.t.Helper()
	if err := os.MkdirAll(f.cfgDir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfgDir, "config.json"), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// fakeOMP puts an omp stand-in first (and only) on PATH. It answers
// `plugin list --json` from a marker file and records `plugin install`.
func (f *installFixture) fakeOMP(pluginInstalled, failInstall bool) (dir string) {
	f.t.Helper()
	dir = filepath.Join(f.t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	installBody := "\t: > \"$dir/plugin-installed\"\n\techo \"fake omp: installed $3\"\n"
	if failInstall {
		installBody = "\techo 'registry unreachable' >&2\n\texit 3\n"
	}
	script := `#!/bin/sh
dir="${0%/*}"
case "$1 $2" in
"plugin list")
	if [ -f "$dir/plugin-installed" ]; then
		echo '{"npm":[{"name":"omp-tokenmunchers","version":"0.2.0"}]}'
	else
		echo '{"npm":[]}'
	fi
	;;
"plugin install")
	echo "$*" >> "$dir/calls.log"
` + installBody + `	;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "omp"), []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if pluginInstalled {
		if err := os.WriteFile(filepath.Join(dir, "plugin-installed"), nil, 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.t.Setenv("PATH", dir)
	return dir
}

func (f *installFixture) agentConfigPath() string {
	return filepath.Join(f.home, ".omp", "agent", "tokenmunchers", "config.json")
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func TestInstallSetsUpKeyServiceAndShell(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)

	code, out, errOut := f.run("", "install", "--yes")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	disk, err := config.LoadFile(f.paths())
	if err != nil || disk.APIKey != f.key {
		t.Fatalf("the env key must be saved for the background service: %v", err)
	}
	if all := out + errOut; strings.Contains(all, f.key) || strings.Contains(all, "/_tm/") {
		t.Errorf("install printed a secret:\n%s", all)
	}
	if got := strings.Join(f.svc.calls, ","); got != "install,start" {
		t.Errorf("service calls = %s, want install,start", got)
	}
	if st, err := shellrc.Inspect(f.bashProfile()); err != nil || !st.Managed {
		t.Errorf("install should add the managed block (state=%+v, err=%v)", st, err)
	}
	for _, want := range []string{"$TOKENMUNCHERS_KEY", "munch is running on http://" + f.listen + " (systemd-user)", "Open a new terminal"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	// Running it again restarts the daemon and changes nothing else.
	f.svc.calls = nil
	code, out, errOut = f.run("", "install", "--yes")
	if code != 0 {
		t.Fatalf("second install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := strings.Join(f.svc.calls, ","); got != "stop,install,start" {
		t.Errorf("second run service calls = %s, want stop,install,start", got)
	}
	for _, want := range []string{"already saved in", "already up to date"} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output is missing %q:\n%s", want, out)
		}
	}
}

func TestInstallPromptsForKey(t *testing.T) {
	f := newInstallFixture(t)

	// The first line is the key; the shell question then hits EOF and takes its default.
	code, out, errOut := f.run(f.key+"\n", "install")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "Paste your TokenMunchers API key") {
		t.Errorf("install should prompt for the key:\n%s", out)
	}
	if strings.Contains(out+errOut, f.key) {
		t.Error("install printed the API key")
	}
	if disk, err := config.LoadFile(f.paths()); err != nil || disk.APIKey != f.key {
		t.Errorf("the pasted key must be saved (err=%v)", err)
	}
}

func TestInstallWithoutKeyFailsBeforeTouchingAnything(t *testing.T) {
	f := newInstallFixture(t)
	for _, args := range [][]string{{"install", "--yes"}, {"install"}} {
		code, _, errOut := f.run("", args...)
		if code == 0 {
			t.Fatalf("%v succeeded without any API key", args)
		}
		if !strings.Contains(errOut, "munch login") || !strings.Contains(errOut, "TOKENMUNCHERS_KEY") {
			t.Errorf("%v error should name the ways to supply a key: %s", args, errOut)
		}
		if len(f.svc.calls) != 0 {
			t.Errorf("%v touched the service without a key: %v", args, f.svc.calls)
		}
	}
}

func TestInstallRejectsMalformedEnvKey(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_API_KEY", "nope")
	code, _, errOut := f.run("", "install", "--yes")
	if code == 0 || !strings.Contains(errOut, "TOKENMUNCHERS_API_KEY") || len(f.svc.calls) != 0 {
		t.Fatalf("malformed env key must stop install (code=%d, calls=%v): %s", code, f.svc.calls, errOut)
	}
}

func TestInstallFailsWhenDaemonNeverAnswers(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	t.Setenv("TMPROXY_LISTEN", "127.0.0.1:1") // nothing listens here
	old := installHealthTimeout
	installHealthTimeout = 300 * time.Millisecond
	t.Cleanup(func() { installHealthTimeout = old })

	code, _, errOut := f.run("", "install", "--yes")
	if code == 0 {
		t.Fatal("install must fail when the daemon never answers")
	}
	for _, want := range []string{"did not answer", "munch status", "journalctl --user -u tmproxy"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("error is missing %q: %s", want, errOut)
		}
	}
	if _, err := os.Stat(f.bashProfile().Path); !os.IsNotExist(err) {
		t.Error("the shell profile must not be touched after a failed start")
	}
}

func TestInstallToleratesStartErrorWhenDaemonIsUp(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	f.svc.startErr = errors.New("service already loaded")
	if code, out, errOut := f.run("", "install", "--yes", "--no-shell"); code != 0 {
		t.Fatalf("a Start error must not matter once the daemon answers (code=%d): out=%s err=%s", code, out, errOut)
	}
}

func TestInstallWithoutAutoStartBackend(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	f.svc.backend = "pidfile"
	f.svc.installErr = errors.New("not supported")

	code, out, errOut := f.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("a backend that cannot auto-start must not fail install (code=%d): out=%s err=%s", code, out, errOut)
	}
	for _, want := range []string{"auto-start is not available", "run `munch start`"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if got := strings.Join(f.svc.calls, ","); got != "install,start" {
		t.Errorf("service calls = %s, want install,start", got)
	}
}

func TestInstallEnvFlags(t *testing.T) {
	if got := installEnvFlags("", "", ""); len(got) != 0 {
		t.Errorf("no overrides should yield no flags, got %v", got)
	}
	got := installEnvFlags("rel/cfg", "/abs/state", " 127.0.0.1:9000 ")
	want := []envFlag{{"--config-dir", absPath("rel/cfg")}, {"--state-dir", "/abs/state"}, {"--listen", "127.0.0.1:9000"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("installEnvFlags = %v, want %v", got, want)
	}
	if !filepath.IsAbs(got[0].value) {
		t.Errorf("a profile line cannot rely on the working directory, got %q", got[0].value)
	}
}

func TestInstallPathOverridesLeaveProfileAlone(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)

	code, out, errOut := f.run("", "install", "--yes", "--config-dir", f.cfgDir, "--state-dir", f.stateDir, "--listen", f.listen)
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if _, err := os.Stat(f.bashProfile().Path); !os.IsNotExist(err) {
		t.Errorf("path overrides must keep install out of the profile (stat err=%v)", err)
	}
	for _, want := range []string{
		"profile not modified",
		"without --config-dir, --state-dir, --listen",
		`eval "$(munch env --check --config-dir ` + quoteArg("sh", f.cfgDir) + ` --state-dir ` + quoteArg("sh", f.stateDir) + ` --listen ` + f.listen + `)"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	// A single override is enough, and only that flag is repeated.
	code, out, errOut = f.run("", "install", "--yes", "--listen", f.listen)
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if _, err := os.Stat(f.bashProfile().Path); !os.IsNotExist(err) {
		t.Errorf("--listen must keep install out of the profile (stat err=%v)", err)
	}
	if want := `eval "$(munch env --check --listen ` + f.listen + `)"`; !strings.Contains(out, want) {
		t.Errorf("output is missing %q:\n%s", want, out)
	}
}

func TestInstallShellNotWired(t *testing.T) {
	cases := []struct {
		name     string
		shell    string
		args     []string
		wantStep string
		wantLine string
	}{
		{"--no-shell", "/bin/bash", []string{"install", "--yes", "--no-shell"}, "(--no-shell)", `eval "$(munch env --check)"`},
		{"unsupported shell", "/bin/dash", []string{"install", "--yes"}, "only edits bash, zsh and fish", `eval "$(munch env --check)"`},
		{"powershell", "/usr/bin/pwsh", []string{"install", "--yes"}, "only edits bash, zsh and fish", "munch env --shell powershell | Out-String | Invoke-Expression"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			t.Setenv("TOKENMUNCHERS_KEY", f.key)
			t.Setenv("SHELL", tc.shell)

			code, out, errOut := f.run("", tc.args...)
			if code != 0 {
				t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
			}
			for _, want := range []string{tc.wantStep, tc.wantLine} {
				if !strings.Contains(out, want) {
					t.Errorf("output is missing %q:\n%s", want, out)
				}
			}
			if entries, err := os.ReadDir(f.home); err != nil || len(entries) != 0 {
				t.Errorf("no profile may be written (entries=%v, err=%v)", entries, err)
			}
		})
	}
}

func TestInstallKeepsExistingProfileContent(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	rc := f.bashProfile().Path

	// A profile that already runs munch env by hand is left alone.
	handwritten := "export FOO=1\neval \"$(munch env --check)\"\n"
	if err := os.WriteFile(rc, []byte(handwritten), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := f.run("", "install", "--yes")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got, _ := os.ReadFile(rc); string(got) != handwritten {
		t.Errorf("a hand-written hook must stay untouched, profile is now:\n%s", got)
	}
	if !strings.Contains(out, "already wired") || !strings.Contains(out, "set up in "+rc) {
		t.Errorf("install should report the existing hook:\n%s", out)
	}

	// Otherwise the block is appended and everything else stays byte for byte.
	existing := "export FOO=1\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := f.run("", "install", "--yes"); code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	got, _ := os.ReadFile(rc)
	if !strings.HasPrefix(string(got), existing) || len(got) <= len(existing) {
		t.Errorf("the managed block must be appended after the existing content:\n%s", got)
	}
}

func TestInstallOMPChoosesPlugin(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	ompDir := f.fakeOMP(false, false)

	code, out, errOut := f.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	calls, err := os.ReadFile(filepath.Join(ompDir, "calls.log"))
	if err != nil || strings.TrimSpace(string(calls)) != "plugin install omp-tokenmunchers" {
		t.Errorf("omp-tokenmunchers should be installed once (log=%q, err=%v)", calls, err)
	}
	agent := readJSONMap(t, f.agentConfigPath())
	if agent["apiKey"] != f.key {
		t.Error("the plugin config should be seeded with the API key")
	}
	if _, ok := agent["ingestUrl"]; ok {
		t.Error("the default ingest URL must not be written to the plugin config")
	}
	if disk, err := config.LoadFile(f.paths()); err != nil || !disk.OMPViaPlugin {
		t.Errorf("omp_via_plugin should be saved (err=%v)", err)
	}
	if strings.Contains(out+errOut, f.key) {
		t.Error("install printed the API key")
	}

	// munch env now carries the wrapper.
	if _, envOut, _ := runApp("", "env", "--check=false", "--shell", "sh"); !strings.Contains(envOut, "omp() {") {
		t.Errorf("env should define the omp wrapper once omp-tm is chosen:\n%s", envOut)
	}
}

func TestInstallOMPSeedsCustomIngestURL(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	f.writeConfig(`{"ingest_url": "https://ingest.example.test/v1/ingest"}`)
	f.fakeOMP(false, false)

	if code, out, errOut := f.run("", "install", "--yes", "--no-shell"); code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := readJSONMap(t, f.agentConfigPath())["ingestUrl"]; got != "https://ingest.example.test/v1/ingest" {
		t.Errorf("a custom ingest URL should reach the plugin, got %v", got)
	}
}

func TestInstallOMPLeavesExistingPluginKey(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	ompDir := f.fakeOMP(true, false)
	if err := os.MkdirAll(filepath.Dir(f.agentConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.agentConfigPath(), []byte(`{"apiKey": "someone-elses-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := f.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := readJSONMap(t, f.agentConfigPath())["apiKey"]; got != "someone-elses-key" {
		t.Errorf("an existing plugin key must be kept, got %v", got)
	}
	if !strings.Contains(out, "already has an API key") {
		t.Errorf("install should say the plugin key was kept:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ompDir, "calls.log")); !os.IsNotExist(err) {
		t.Error("an installed plugin must not be installed again")
	}
	if disk, err := config.LoadFile(f.paths()); err != nil || !disk.OMPViaPlugin {
		t.Errorf("omp_via_plugin should be saved (err=%v)", err)
	}
}

func TestInstallOMPDeclined(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	f.fakeOMP(true, false)
	f.writeConfig(`{"omp_via_plugin": true}`)

	code, out, errOut := f.run("n\n", "install", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if disk, err := config.LoadFile(f.paths()); err != nil || disk.OMPViaPlugin {
		t.Errorf("declining omp-tm must clear omp_via_plugin (err=%v)", err)
	}
	for _, want := range []string{"counted twice", "omp plugin disable omp-tokenmunchers"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the double-counting warning is missing %q: %s", want, errOut)
		}
	}
	if _, err := os.Stat(f.agentConfigPath()); !os.IsNotExist(err) {
		t.Error("declining omp-tm must not write the plugin config")
	}
}

func TestInstallOMPPluginInstallFailureIsOnlyAWarning(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	f.fakeOMP(false, true)

	code, out, errOut := f.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("a failing plugin install must not fail install (code=%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "omp-tm is not set up") {
		t.Errorf("the failure should be reported: %s", errOut)
	}
	if disk, err := config.LoadFile(f.paths()); err != nil || disk.OMPViaPlugin {
		t.Errorf("omp_via_plugin must stay off when the plugin is not set up (err=%v)", err)
	}
}

func TestUninstallRemovesProfileBlockAndExplainsLeftovers(t *testing.T) {
	f := newInstallFixture(t)
	profile := f.bashProfile()
	if _, err := shellrc.Install(profile, "/opt/munch"); err != nil {
		t.Fatal(err)
	}
	block, err := os.ReadFile(profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	handwritten := "eval \"$(munch env --check)\"\n"
	if err := os.WriteFile(profile.Path, append(block, handwritten...), 0o644); err != nil {
		t.Fatal(err)
	}
	f.writeConfig(`{"omp_via_plugin": true}`)

	code, out, errOut := f.run("", "uninstall")
	if code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := strings.Join(f.svc.calls, ","); got != "uninstall" {
		t.Errorf("service calls = %s, want uninstall", got)
	}
	st, err := shellrc.Inspect(profile)
	if err != nil || st.Managed || !st.Unmanaged {
		t.Errorf("only the managed block should go (state=%+v, err=%v)", st, err)
	}
	for _, want := range []string{
		"Removed munch from " + profile.Path,
		"remove that line manually",
		"omp-tokenmunchers plugin was left installed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestInstallOMPProxiedSaysOllamaIsLeftOut(t *testing.T) {
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	f.fakeOMP(true, false)
	f.writeConfig(`{"omp_via_plugin": true}`)

	code, out, errOut := f.run("n\n", "install")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	for _, want := range []string{"OMP is proxied", "no OLLAMA_HOST", "local Ollama directly"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reports through omp-tm") {
		t.Errorf("omp-tm was declined, so the summary must not claim it:\n%s", out)
	}
}

// newExecFixture is an install fixture whose daemon already holds the local
// key, so `munch exec` sees a healthy proxy. The fake tools it runs call
// `env`, so the child gets the system PATH instead of the fixture's empty one.
func newExecFixture(t *testing.T) *installFixture {
	t.Helper()
	f := newInstallFixture(t)
	if _, err := localauth.LoadOrCreate(f.cfgDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", origPATH)
	return f
}

// writeFakeTool writes a script that prints its environment and returns its path.
func writeFakeTool(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecOMPDropsOllamaHostOnly(t *testing.T) {
	f := newExecFixture(t)
	t.Setenv("OLLAMA_HOST", "http://parent-ollama:11434") // exported by the user's shell
	ompPath := writeFakeTool(t, "omp", fakeOMPScript)
	otherPath := writeFakeTool(t, "othertool", fakeOtherScript)

	code, out, errOut := f.run("", "exec", "--", ompPath, "one")
	if code != 0 {
		t.Fatalf("exec omp failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "OMP_RAN") || !strings.Contains(out, "ARG=one\n") {
		t.Fatalf("omp did not run with its arguments:\n%s", out)
	}
	if strings.Contains(out, "OLLAMA_HOST=") {
		t.Errorf("omp must not see OLLAMA_HOST, neither munch's nor the inherited one:\n%s", out)
	}
	if !strings.Contains(out, "ANTHROPIC_BASE_URL=http://"+f.listen+"/") {
		t.Errorf("omp should still be proxied for the other providers:\n%s", out)
	}
	if strings.Contains(errOut, "omp-tm") {
		t.Errorf("the omp-tm note belongs to omp_via_plugin only: %s", errOut)
	}

	// Any other command gets the proxy's OLLAMA_HOST in place of the inherited one.
	code, out, errOut = f.run("", "exec", "--", otherPath)
	if code != 0 {
		t.Fatalf("exec othertool failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "OLLAMA_HOST=http://"+f.listen+"/") || strings.Contains(out, "parent-ollama") {
		t.Errorf("a non-omp command should get the proxy OLLAMA_HOST:\n%s", out)
	}
}

func TestExecOMPViaPluginDropsEveryProxyVariable(t *testing.T) {
	f := newExecFixture(t)
	f.writeConfig(`{"omp_via_plugin": true}`)
	t.Setenv("OLLAMA_HOST", "http://parent-ollama:11434")
	t.Setenv("OPENAI_BASE_URL", "http://parent-openai.example/v1")
	ompPath := writeFakeTool(t, "omp", fakeOMPScript)

	code, out, errOut := f.run("", "exec", "--", ompPath)
	if code != 0 {
		t.Fatalf("exec omp failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	for _, name := range envNames() {
		if strings.Contains(out, name+"=") {
			t.Errorf("%s reached omp although it reports through omp-tm:\n%s", name, out)
		}
	}
	if !strings.Contains(errOut, "omp-tm") {
		t.Errorf("exec should say omp runs without the proxy variables: %s", errOut)
	}
}

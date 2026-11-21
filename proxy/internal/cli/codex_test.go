package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
)

// codexChatGPT is what `codex login status` runs when Codex is signed in with
// ChatGPT.
const codexChatGPT = `echo "Logged in using ChatGPT" >&2`

// codexTypical looks like a config Codex itself maintains.
const codexTypical = "model = \"gpt-5\"\n\n[projects.\"/work/app\"]\ntrust_level = \"trusted\"\n"

// codexFixture is an install fixture with a stand-in codex first and only on
// PATH, and a private Codex home in $CODEX_HOME.
type codexFixture struct {
	*installFixture

	// bin holds the stand-in codex and its calls.log.
	bin       string
	codexHome string
}

// newCodexFixture runs `codex login status` as statusBody. Any other codex
// command exits 2.
func newCodexFixture(t *testing.T, statusBody string) *codexFixture {
	t.Helper()
	f := newInstallFixture(t)
	t.Setenv("TOKENMUNCHERS_KEY", f.key)
	c := &codexFixture{
		installFixture: f,
		bin:            filepath.Join(t.TempDir(), "bin"),
		codexHome:      filepath.Join(t.TempDir(), "codex-home"),
	}
	for _, dir := range []string{c.bin, c.codexHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c.writeCodex(statusBody)
	t.Setenv("PATH", c.bin)
	t.Setenv("CODEX_HOME", c.codexHome)
	return c
}

func (c *codexFixture) writeCodex(statusBody string) {
	c.t.Helper()
	script := "#!/bin/sh\n" +
		"dir=\"${0%/*}\"\n" +
		"echo \"$*\" >> \"$dir/calls.log\"\n" +
		"if [ \"$1 $2\" = \"login status\" ]; then\n" +
		statusBody + "\n" +
		"exit 0\n" +
		"fi\n" +
		"exit 2\n"
	if err := os.WriteFile(filepath.Join(c.bin, "codex"), []byte(script), 0o755); err != nil {
		c.t.Fatal(err)
	}
}

func (c *codexFixture) configPath() string { return filepath.Join(c.codexHome, "config.toml") }

func (c *codexFixture) writeCodexConfig(content string) {
	c.t.Helper()
	if err := os.WriteFile(c.configPath(), []byte(content), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func (c *codexFixture) readCodexConfig() string {
	c.t.Helper()
	data, err := os.ReadFile(c.configPath())
	if err != nil {
		c.t.Fatal(err)
	}
	return string(data)
}

// codexCalls returns what the stand-in codex was asked to do, one call a line.
func (c *codexFixture) codexCalls() string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.bin, "calls.log"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		c.t.Fatal(err)
	}
	return string(data)
}

// routeURL is the Codex provider base_url for the current local key.
func (c *codexFixture) routeURL() string {
	c.t.Helper()
	key, err := localauth.Load(c.cfgDir)
	if err != nil {
		c.t.Fatal(err)
	}
	return "http://" + c.listen + key.RoutePrefix() + "/chatgpt/backend-api/codex"
}

// noSecrets fails the test if output shows the API key or a private route.
func (c *codexFixture) noSecrets(output string) {
	c.t.Helper()
	if strings.Contains(output, c.key) || strings.Contains(output, "/_tm/") {
		c.t.Errorf("output shows a secret:\n%s", output)
	}
}

func TestInstallRoutesCodexSignedInWithChatGPT(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	cfg := c.readCodexConfig()
	for _, want := range []string{
		`model_provider = "munch_chatgpt"`,
		"[model_providers.munch_chatgpt]",
		`name = "Munch"`,
		`base_url = "` + c.routeURL() + `"`,
		`wire_api = "responses"`,
		"requires_openai_auth = true",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.toml is missing %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "chatgpt_base_url") {
		t.Errorf("chatgpt_base_url must stay at Codex's default:\n%s", cfg)
	}
	info, err := os.Stat(c.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode = %v, want 0600: it holds the private route", info.Mode().Perm())
	}
	for _, want := range []string{
		"added the Munch provider to " + c.configPath(),
		"Codex signed in with ChatGPT is routed through it too (Munch provider in " + c.configPath() + ")",
		"start a new Codex process",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	c.noSecrets(out + errOut)
	if got := strings.TrimSpace(c.codexCalls()); got != "login status" {
		t.Errorf("codex was run as %q, want a single `login status`", got)
	}
}

func TestInstallAsksAboutCodexAfterTheShell(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)

	// The first answer is for the shell profile, the second for Codex.
	code, out, errOut := c.run("y\nn\n", "install")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	shellAt := strings.Index(out, "Add munch to ")
	codexAt := strings.Index(out, "Add the Munch provider to ")
	if shellAt < 0 || codexAt < shellAt {
		t.Errorf("the Codex question must come after the shell question (shell at %d, Codex at %d):\n%s", shellAt, codexAt, out)
	}
	profile, err := os.ReadFile(c.bashProfile().Path)
	if err != nil || !strings.Contains(string(profile), "# >>> munch >>>") {
		t.Errorf("the shell answer was not honoured (err=%v)", err)
	}
	if _, err := os.Stat(c.configPath()); !os.IsNotExist(err) {
		t.Errorf("declining must leave the Codex config alone (stat err=%v)", err)
	}
	for _, want := range []string{"config left as is", "Codex signed in with ChatGPT is not routed through it", "`munch install` again"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "is routed through it too") {
		t.Errorf("a declined route must not be reported as active:\n%s", out)
	}
}

func TestInstallNoCodexLeavesCodexAlone(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)

	code, out, errOut := c.run("", "install", "--yes", "--no-shell", "--no-codex")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "config not modified (--no-codex)") {
		t.Errorf("output should say Codex was skipped on request:\n%s", out)
	}
	if _, err := os.Stat(c.configPath()); !os.IsNotExist(err) {
		t.Errorf("--no-codex must not write a Codex config (stat err=%v)", err)
	}
	if calls := c.codexCalls(); calls != "" {
		t.Errorf("--no-codex must not even run codex, got %q", calls)
	}
}

func TestInstallLeavesCodexAloneWithoutChatGPTSignIn(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"API key", `echo "Logged in using an API key - sk-proj-***SECRETKEY" >&2`},
		{"signed out", `echo "Not logged in" >&2; exit 1`},
		{"unrecognised output", `echo "something else entirely"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCodexFixture(t, tc.body)
			code, out, errOut := c.run("", "install", "--yes", "--no-shell")
			if code != 0 {
				t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
			}
			if !strings.Contains(out, "not signed in with ChatGPT") {
				t.Errorf("output should say why Codex was left alone:\n%s", out)
			}
			if _, err := os.Stat(c.configPath()); !os.IsNotExist(err) {
				t.Errorf("a Codex without ChatGPT sign-in must not be configured (stat err=%v)", err)
			}
			if strings.Contains(out+errOut, "SECRETKEY") {
				t.Errorf("the sign-in status output must never be shown:\n%s%s", out, errOut)
			}
		})
	}
}

func TestInstallBoundsTheCodexSignInCheck(t *testing.T) {
	old := codexStatusTimeout
	codexStatusTimeout = 300 * time.Millisecond
	t.Cleanup(func() { codexStatusTimeout = old })
	c := newCodexFixture(t, `while :; do :; done`)

	start := time.Now()
	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("a hung codex must not fail install (code=%d): out=%s err=%s", code, out, errOut)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("install took %v, want the sign-in check cut off", elapsed)
	}
	if !strings.Contains(errOut, "could not tell whether Codex is signed in with ChatGPT") {
		t.Errorf("the warning is missing:\n%s", errOut)
	}
	if _, err := os.Stat(c.configPath()); !os.IsNotExist(err) {
		t.Errorf("an unverified Codex must not be configured (stat err=%v)", err)
	}
}

func TestReinstallKeepsTheCodexRouteCurrent(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	first := c.readCodexConfig()
	firstInfo, err := os.Stat(c.configPath())
	if err != nil {
		t.Fatal(err)
	}

	// The same proxy again: nothing to change, nothing rewritten.
	code, out, errOut = c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("second install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "already routed through munch ("+c.configPath()+")") {
		t.Errorf("the second run should say Codex is already routed:\n%s", out)
	}
	secondInfo, err := os.Stat(c.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if c.readCodexConfig() != first || !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Error("an up-to-date Codex config must not be rewritten")
	}

	// A new local key is a new private route: munch's block follows it.
	if err := os.Remove(filepath.Join(c.cfgDir, localauth.FileName)); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("third install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if want := "pointed the Munch provider in " + c.configPath() + " at this proxy"; !strings.Contains(out, want) {
		t.Errorf("output is missing %q:\n%s", want, out)
	}
	second := c.readCodexConfig()
	if second == first || !strings.Contains(second, c.routeURL()) {
		t.Errorf("the Codex config did not follow the new route:\n%s", second)
	}
	if strings.Count(second, "# >>> munch codex provider >>>") != 1 || strings.Count(second, "model_provider = ") != 1 {
		t.Errorf("the refreshed config has duplicate blocks:\n%s", second)
	}
	c.noSecrets(out + errOut)
}

func TestInstallMigratesTheOldChatGPTBaseURL(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	legacy := "http://127.0.0.1:8787/_tm/" + strings.Repeat("0a", 32) + "/chatgpt/backend-api/"
	c.writeCodexConfig("model = \"gpt-5\"\nchatgpt_base_url = \"" + legacy + "\"\n\n[projects.\"/work/app\"]\ntrust_level = \"trusted\"\n")

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	cfg := c.readCodexConfig()
	if strings.Contains(cfg, "chatgpt_base_url") {
		t.Errorf("the old plain-HTTP chatgpt_base_url must go:\n%s", cfg)
	}
	if !strings.Contains(cfg, codexTypical) || !strings.Contains(cfg, c.routeURL()) {
		t.Errorf("the other settings must stay and the provider must be added:\n%s", cfg)
	}
	for _, want := range []string{
		"added the Munch provider to " + c.configPath(),
		"removed the old plain-HTTP chatgpt_base_url from " + c.configPath(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	c.noSecrets(out + errOut)

	// Nothing is left to do on the next run, and uninstall does not bring the old line back.
	if _, out, _ := c.run("", "install", "--yes", "--no-shell"); !strings.Contains(out, "already routed through munch") {
		t.Errorf("the second run should find nothing to do:\n%s", out)
	}
	if code, out, errOut := c.run("", "uninstall"); code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := c.readCodexConfig(); got != codexTypical {
		t.Errorf("config after uninstall =\n%s\nwant the original without the old line", got)
	}
}

func TestInstallRecognizesTheDocumentedManualProvider(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	if _, err := localauth.LoadOrCreate(c.cfgDir); err != nil {
		t.Fatal(err)
	}
	manual := "model = \"gpt-5\"\nmodel_provider = \"munch_chatgpt\"\n\n[model_providers.munch_chatgpt]\nname = \"Munch\"\nbase_url = \"" +
		c.routeURL() + "\"\nwire_api = \"responses\"\nrequires_openai_auth = true\n"
	c.writeCodexConfig(manual)

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := c.readCodexConfig(); got != manual {
		t.Errorf("a hand-written setup that already routes must stay as it is:\n%s", got)
	}
	for _, want := range []string{
		"already routed through munch by the provider you added to " + c.configPath(),
		"is routed through it too",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	c.noSecrets(out + errOut)

	// Uninstall never deletes what the user wrote, but says it still routes Codex.
	code, out, errOut = c.run("", "uninstall")
	if code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := c.readCodexConfig(); got != manual {
		t.Errorf("uninstall changed a hand-written setup:\n%s", got)
	}
	for _, want := range []string{
		c.configPath() + " still has munch settings that were written by hand or edited",
		`model_provider = "munch_chatgpt"`,
		"Codex fails to connect until you revert them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("uninstall output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Removed munch's settings") {
		t.Errorf("nothing was removed, so uninstall must not say so:\n%s", out)
	}
	c.noSecrets(out + errOut)

	// A route that points at an older proxy address is repointed in place.
	stale := strings.Replace(manual, c.routeURL(), "http://127.0.0.1:1/_tm/"+strings.Repeat("00", 32)+"/chatgpt/backend-api/codex", 1)
	c.writeCodexConfig(stale)
	code, out, errOut = c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := c.readCodexConfig(); got != manual {
		t.Errorf("only the base_url of a hand-written setup may change:\n%s", got)
	}
	if want := "pointed the Munch provider in " + c.configPath() + " at this proxy"; !strings.Contains(out, want) {
		t.Errorf("output is missing %q:\n%s", want, out)
	}
}

func TestInstallLeavesOtherCodexSetupsAlone(t *testing.T) {
	cases := []struct {
		name    string
		content string
		reason  string
	}{
		{"another model provider", "model_provider = \"azure\"\n", "another model provider"},
		{"munch_chatgpt table that munch did not write", "[model_providers.munch_chatgpt]\nname = \"Mine\"\n", "did not write"},
		{"custom ChatGPT backend", "chatgpt_base_url = \"https://proxy.example.test/backend-api/\"\n", "custom backend"},
		{"invalid TOML", "model = \n", "not valid TOML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCodexFixture(t, codexChatGPT)
			c.writeCodexConfig(tc.content)

			code, out, errOut := c.run("", "install", "--yes", "--no-shell")
			if code != 0 {
				t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
			}
			if got := c.readCodexConfig(); got != tc.content {
				t.Errorf("a refused Codex config was modified:\n%s", got)
			}
			if want := "left " + c.configPath() + " as is: "; !strings.Contains(out, want) || !strings.Contains(out, tc.reason) {
				t.Errorf("output should say the config was left alone because of %q:\n%s", tc.reason, out)
			}
			if strings.Contains(out, "routed through it too") || strings.Contains(out, "not routed through it") {
				t.Errorf("a refused config is neither active nor pending:\n%s", out)
			}
			if strings.Contains(out+errOut, "example.test") {
				t.Errorf("the output echoes a value from the config:\n%s%s", out, errOut)
			}
			c.noSecrets(out + errOut)
		})
	}
}

func TestInstallNeverConfiguresCodexForACustomChatGPTUpstream(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	c.writeConfig(`{"routes": {"chatgpt": {"upstream": "https://chatgpt.example.test"}}}`)

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	for _, want := range []string{"uses a custom upstream", "munch will not configure Codex to send its sign-in there"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(c.configPath()); !os.IsNotExist(err) {
		t.Errorf("Codex must not be pointed at a proxy that forwards to a custom upstream (stat err=%v)", err)
	}
	c.noSecrets(out + errOut)
}

func TestInstallSkipsAMissingCodexHome(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	if err := os.RemoveAll(c.codexHome); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if want := c.codexHome + " is not a directory"; !strings.Contains(out, want) {
		t.Errorf("output is missing %q:\n%s", want, out)
	}
	if _, err := os.Stat(c.codexHome); !os.IsNotExist(err) {
		t.Errorf("install must not create a Codex home (stat err=%v)", err)
	}
}

func TestInstallKeepsExistingCodexSettingsAndMakesTheFilePrivate(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	c.writeCodexConfig(codexTypical)

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if cfg := c.readCodexConfig(); !strings.Contains(cfg, codexTypical) || !strings.Contains(cfg, `model_provider = "munch_chatgpt"`) {
		t.Errorf("existing settings must stay byte for byte next to the new blocks:\n%s", cfg)
	}
	info, err := os.Stat(c.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode = %v, want 0600 once it holds the private route", info.Mode().Perm())
	}
}

func TestInstallEditsASymlinkedCodexConfigInPlace(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	target := filepath.Join(t.TempDir(), "dotfiles-codex.toml")
	if err := os.WriteFile(target, []byte(codexTypical), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, c.configPath()); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if info, err := os.Lstat(c.configPath()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("install replaced the symlink (err=%v)", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), codexTypical) || !strings.Contains(string(data), c.routeURL()) {
		t.Errorf("the file behind the symlink should hold the old settings and the route:\n%s", data)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the file behind the symlink has mode %v, want 0600", info.Mode().Perm())
	}

	if code, out, errOut := c.run("", "uninstall"); code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if info, err := os.Lstat(c.configPath()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("uninstall replaced the symlink (err=%v)", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != codexTypical {
		t.Errorf("the file behind the symlink after uninstall = %q (err=%v), want the original", got, err)
	}
}

func TestInstallReportsAnUnwritableCodexHome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	c := newCodexFixture(t, codexChatGPT)
	if err := os.Chmod(c.codexHome, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(c.codexHome, 0o700) })

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("a Codex write failure must not fail install (code=%d): out=%s err=%s", code, out, errOut)
	}
	if want := "could not update " + c.configPath(); !strings.Contains(errOut, want) {
		t.Errorf("the warning should name the file (want %q):\n%s", want, errOut)
	}
	if !strings.Contains(out, "Codex signed in with ChatGPT is not routed through it") {
		t.Errorf("the summary should say Codex is not routed:\n%s", out)
	}
	if entries, err := os.ReadDir(c.codexHome); err != nil || len(entries) != 0 {
		t.Errorf("nothing may be left in the Codex home: %v (err=%v)", entries, err)
	}
}

func TestInstallAndUninstallUseTheDefaultCodexHome(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	t.Setenv("CODEX_HOME", "")
	defaultHome := filepath.Join(c.home, ".codex")
	if err := os.MkdirAll(defaultHome, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(defaultHome, "config.toml")

	code, out, errOut := c.run("", "install", "--yes", "--no-shell")
	if code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil || !strings.Contains(string(data), `model_provider = "munch_chatgpt"`) {
		t.Fatalf("~/.codex/config.toml should hold the route (err=%v):\n%s", err, data)
	}

	// Uninstall also cleans the standard location when CODEX_HOME points elsewhere by then.
	t.Setenv("CODEX_HOME", c.codexHome)
	code, out, errOut = c.run("", "uninstall")
	if code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if want := "Removed munch's settings from the Codex config " + cfgPath; !strings.Contains(out, want) {
		t.Errorf("output is missing %q:\n%s", want, out)
	}
	if got, err := os.ReadFile(cfgPath); err != nil || len(got) != 0 {
		t.Errorf("config after uninstall = %q (err=%v), want an empty file", got, err)
	}
}

func TestUninstallRemovesWhatInstallAddedToCodex(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	c.writeCodexConfig(codexTypical)
	if code, out, errOut := c.run("", "install", "--yes", "--no-shell"); code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}

	code, out, errOut := c.run("", "uninstall")
	if code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := c.readCodexConfig(); got != codexTypical {
		t.Errorf("config after uninstall =\n%s\nwant the original byte for byte", got)
	}
	if want := "Removed munch's settings from the Codex config " + c.configPath(); !strings.Contains(out, want) {
		t.Errorf("output is missing %q:\n%s", want, out)
	}
	if strings.Contains(out, "still has munch settings") {
		t.Errorf("nothing should be left behind:\n%s", out)
	}
	c.noSecrets(out + errOut)
}

func TestUninstallLeavesAnEmptiedCodexConfigInPlace(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	if code, out, errOut := c.run("", "install", "--yes", "--no-shell"); code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}

	if code, out, errOut := c.run("", "uninstall"); code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	// Nothing records whether the file existed before install, and an empty
	// config is harmless, so it is not deleted.
	got, err := os.ReadFile(c.configPath())
	if err != nil {
		t.Fatalf("uninstall deleted the emptied config: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("config after uninstall = %q, want it empty", got)
	}
}

func TestUninstallDoesNotClobberAnEditedProvider(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	c.writeCodexConfig(codexTypical)
	if code, out, errOut := c.run("", "install", "--yes", "--no-shell"); code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	c.writeCodexConfig(strings.Replace(c.readCodexConfig(), `name = "Munch"`, `name = "Mine"`, 1))

	code, out, errOut := c.run("", "uninstall")
	if code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	cfg := c.readCodexConfig()
	if strings.Contains(cfg, `model_provider = "munch_chatgpt"`) {
		t.Errorf("munch's own selection should be gone, so Codex falls back to its default provider:\n%s", cfg)
	}
	if !strings.Contains(cfg, `name = "Mine"`) || !strings.Contains(cfg, codexTypical) {
		t.Errorf("the user's edit and the rest of the file must stay:\n%s", cfg)
	}
	for _, want := range []string{
		"Removed munch's settings from the Codex config " + c.configPath(),
		"still has munch settings that were written by hand or edited",
		"a munch block that was edited by hand",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	c.noSecrets(out + errOut)
}

func TestUninstallKeepsTheProviderWhileCodexStillSelectsIt(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	c.writeCodexConfig(codexTypical)
	if code, out, errOut := c.run("", "install", "--yes", "--no-shell"); code != 0 {
		t.Fatalf("install failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	// The user retyped only the selection line inside munch's block. It still
	// selects munch_chatgpt, so deleting the provider would leave Codex
	// selecting a provider that no longer exists.
	retyped := strings.Replace(c.readCodexConfig(), `model_provider = "munch_chatgpt"`, `model_provider="munch_chatgpt" # mine`, 1)
	c.writeCodexConfig(retyped)

	code, out, errOut := c.run("", "uninstall")
	if code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := c.readCodexConfig(); got != retyped {
		t.Errorf("uninstall must leave a config that still selects its provider untouched:\n%s", got)
	}
	for _, want := range []string{
		c.configPath() + " still has munch settings that were written by hand or edited",
		"a munch block that was edited by hand",
		`model_provider = "munch_chatgpt"`,
		"[model_providers.munch_chatgpt]",
		"Codex fails to connect until you revert them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Removed munch's settings") {
		t.Errorf("nothing was removed, so uninstall must not say so:\n%s", out)
	}
	c.noSecrets(out + errOut)
}

func TestUninstallSaysNothingAboutCodexWithoutMunchSettings(t *testing.T) {
	c := newCodexFixture(t, codexChatGPT)
	c.writeCodexConfig(codexTypical)

	code, out, errOut := c.run("", "uninstall")
	if code != 0 {
		t.Fatalf("uninstall failed (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := c.readCodexConfig(); got != codexTypical {
		t.Errorf("uninstall changed a Codex config without munch settings:\n%s", got)
	}
	if strings.Contains(out+errOut, "Codex") {
		t.Errorf("uninstall should not mention Codex when it has nothing to report:\n%s%s", out, errOut)
	}
	if calls := c.codexCalls(); calls != "" {
		t.Errorf("uninstall must not run codex, got %q", calls)
	}
}

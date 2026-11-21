package codex

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	testCap   = strings.Repeat("ab", 32)
	testURL   = "http://127.0.0.1:8787/_tm/" + testCap + "/chatgpt/backend-api/codex"
	otherURL  = "http://127.0.0.1:9999/_tm/" + strings.Repeat("cd", 32) + "/chatgpt/backend-api/codex"
	legacyURL = "http://127.0.0.1:8787/_tm/" + strings.Repeat("ef", 32) + "/chatgpt/backend-api/"
)

// typicalConfig looks like a config Codex itself maintains.
const typicalConfig = "model = \"gpt-5\"\n" +
	"approval_policy = \"on-request\"\n" +
	"\n" +
	"[projects.\"/work/app\"]\n" +
	"trust_level = \"trusted\"\n" +
	"\n" +
	"[mcp_servers.docs]\n" +
	"command = \"docs-server\"\n" +
	"args = [\"--stdio\", \"--verbose\"]\n"

// selectBlock is the block munch writes at the top of the file.
const selectBlock = "# >>> munch codex select >>>\n" +
	"# Managed by munch install; removed by munch uninstall.\n" +
	"model_provider = \"munch_chatgpt\"\n" +
	"# <<< munch codex select <<<\n"

// providerBlock is the block munch writes at the end of the file.
func providerBlock(url string) string {
	return "# >>> munch codex provider >>>\n" +
		"# Managed by munch install; removed by munch uninstall.\n" +
		"[model_providers.munch_chatgpt]\n" +
		"name = \"Munch\"\n" +
		"base_url = \"" + url + "\"\n" +
		"wire_api = \"responses\"\n" +
		"requires_openai_auth = true\n" +
		"# <<< munch codex provider <<<\n"
}

// manualProvider is the setup the documentation tells users to write by hand.
func manualProvider(url string) string {
	return "model_provider = \"munch_chatgpt\"\n" +
		"\n" +
		"[model_providers.munch_chatgpt]\n" +
		"name = \"Munch\"\n" +
		"base_url = \"" + url + "\"\n" +
		"wire_api = \"responses\"\n" +
		"requires_openai_auth = true\n"
}

// installedConfig is typicalConfig after install.
func installedConfig(url string) string {
	return selectBlock + "\n" + typicalConfig + "\n" + providerBlock(url)
}

func writeText(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func inspect(t *testing.T, path, url string) *Plan {
	t.Helper()
	plan, err := Inspect(path, url)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return plan
}

// install inspects and applies, failing the test if the config is refused.
func install(t *testing.T, path, url string) {
	t.Helper()
	plan := inspect(t, path, url)
	if plan.Status == Blocked {
		t.Fatalf("install is blocked: %s", plan.Reason)
	}
	if err := Apply(plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestInstallCreatesAPrivateConfigWithBothBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	plan := inspect(t, path, testURL)
	if plan.Status != Unrouted || plan.Manual || !plan.Has(AddRoute) || plan.Has(DropLegacyURL) || plan.Has(UpdateRoute) {
		t.Fatalf("plan = status %v, manual %v, actions %v; want Unrouted with AddRoute only", plan.Status, plan.Manual, plan.Actions)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}

	if got, want := readText(t, path), selectBlock+"\n"+providerBlock(testURL); got != want {
		t.Errorf("config.toml =\n%s\nwant\n%s", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600: the file holds the private route", info.Mode().Perm())
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
		t.Errorf("a temporary file was left behind: %v (err=%v)", entries, err)
	}
}

func TestInstallKeepsExistingContentAndRemoveRestoresIt(t *testing.T) {
	path := writeText(t, typicalConfig)
	install(t, path, testURL)

	if got, want := readText(t, path), installedConfig(testURL); got != want {
		t.Errorf("installed config =\n%s\nwant\n%s", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("an existing 0644 config must become 0600 once it holds the route (mode=%v)", info.Mode().Perm())
	}

	res, err := Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removed || len(res.Kept) != 0 {
		t.Errorf("Remove = %+v, want everything removed", res)
	}
	if got := readText(t, path); got != typicalConfig {
		t.Errorf("config after Remove =\n%s\nwant the original back byte for byte", got)
	}
}

func TestRemoveLeavesAnEmptiedConfigInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	install(t, path, testURL)

	res, err := Remove(path)
	if err != nil || !res.Removed {
		t.Fatalf("Remove = %+v, err=%v", res, err)
	}
	// Nothing records whether the file existed before munch: an empty config is
	// harmless, so it stays.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("an emptied config must not be deleted: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("config after Remove = %q, want empty", got)
	}
}

func TestReinstallOfTheCurrentRouteWritesNothing(t *testing.T) {
	path := writeText(t, typicalConfig)
	install(t, path, testURL)
	before := readText(t, path)
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	plan := inspect(t, path, testURL)
	if plan.Status != Routed || plan.Manual || plan.NeedsWrite() {
		t.Fatalf("plan = status %v, manual %v, actions %v; want Routed with nothing to do", plan.Status, plan.Manual, plan.Actions)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if readText(t, path) != before || !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Error("an up-to-date config must not be rewritten")
	}
}

func TestStaleManagedRouteIsRepointed(t *testing.T) {
	path := writeText(t, typicalConfig)
	install(t, path, testURL)

	plan := inspect(t, path, otherURL)
	if plan.Status != Stale || plan.Manual || !plan.Has(UpdateRoute) || plan.Has(AddRoute) {
		t.Fatalf("plan = status %v, manual %v, actions %v; want Stale with UpdateRoute", plan.Status, plan.Manual, plan.Actions)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := readText(t, path), installedConfig(otherURL); got != want {
		t.Errorf("config =\n%s\nwant\n%s", got, want)
	}
	if plan := inspect(t, path, otherURL); plan.Status != Routed {
		t.Errorf("status after repointing = %v, want Routed", plan.Status)
	}
}

func TestHalfRemovedBlocksAreCompleted(t *testing.T) {
	// The user deleted the selection block and kept the provider block.
	path := writeText(t, typicalConfig+"\n"+providerBlock(testURL))
	plan := inspect(t, path, testURL)
	if plan.Status != Stale || plan.Manual || !plan.Has(UpdateRoute) {
		t.Fatalf("plan = status %v, manual %v, actions %v; want Stale with UpdateRoute", plan.Status, plan.Manual, plan.Actions)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := readText(t, path), installedConfig(testURL); got != want {
		t.Errorf("config =\n%s\nwant\n%s", got, want)
	}
}

func TestDocumentedManualProviderIsRecognized(t *testing.T) {
	manual := "model = \"gpt-5\"\n" + manualProvider(testURL)

	t.Run("current route is already routed", func(t *testing.T) {
		path := writeText(t, manual)
		plan := inspect(t, path, testURL)
		if plan.Status != Routed || !plan.Manual || plan.NeedsWrite() {
			t.Fatalf("plan = status %v, manual %v, actions %v; want a hand-written Routed config left alone", plan.Status, plan.Manual, plan.Actions)
		}
	})

	t.Run("stale route is repointed in place and stays the user's", func(t *testing.T) {
		path := writeText(t, manual)
		plan := inspect(t, path, otherURL)
		if plan.Status != Stale || !plan.Manual || !plan.Has(UpdateRoute) || plan.Has(AddRoute) {
			t.Fatalf("plan = status %v, manual %v, actions %v; want Stale with UpdateRoute", plan.Status, plan.Manual, plan.Actions)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(manual, testURL, otherURL, 1)
		if got := readText(t, path); got != want {
			t.Errorf("config =\n%s\nwant only base_url changed:\n%s", got, want)
		}

		// Hand-written settings are never removed, only reported.
		res, err := Remove(path)
		if err != nil {
			t.Fatal(err)
		}
		if res.Removed || len(res.Kept) != 2 {
			t.Errorf("Remove = %+v, want nothing removed and the selection and the table reported", res)
		}
		if got := readText(t, path); got != want {
			t.Errorf("Remove changed a hand-written config:\n%s", got)
		}
	})
}

func TestOldPlainHTTPChatGPTBaseURLIsDropped(t *testing.T) {
	t.Run("while adding the route", func(t *testing.T) {
		rest := "model = \"gpt-5\"\n\n[projects.\"/work/app\"]\ntrust_level = \"trusted\"\n"
		path := writeText(t, "model = \"gpt-5\"\nchatgpt_base_url = \""+legacyURL+"\"\n\n[projects.\"/work/app\"]\ntrust_level = \"trusted\"\n")
		plan := inspect(t, path, testURL)
		if plan.Status != Unrouted || !plan.Has(DropLegacyURL) || !plan.Has(AddRoute) {
			t.Fatalf("plan = status %v, actions %v; want Unrouted with DropLegacyURL and AddRoute", plan.Status, plan.Actions)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		if got, want := readText(t, path), selectBlock+"\n"+rest+"\n"+providerBlock(testURL); got != want {
			t.Errorf("config =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("next to a hand-written route", func(t *testing.T) {
		// Single quotes and a trailing comment are still recognised as the old setup.
		path := writeText(t, "chatgpt_base_url = '"+legacyURL+"'  # old setup\n"+manualProvider(testURL))
		plan := inspect(t, path, testURL)
		if plan.Status != Routed || !plan.Manual || !plan.Has(DropLegacyURL) || plan.Has(UpdateRoute) {
			t.Fatalf("plan = status %v, manual %v, actions %v; want Routed with only DropLegacyURL", plan.Status, plan.Manual, plan.Actions)
		}
		if err := Apply(plan); err != nil {
			t.Fatal(err)
		}
		if got, want := readText(t, path), manualProvider(testURL); got != want {
			t.Errorf("config =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("an old capability is still recognised", func(t *testing.T) {
		// The old URL carries another capability and port than the current proxy.
		path := writeText(t, "chatgpt_base_url = \"http://localhost:1/_tm/"+strings.Repeat("0f", 32)+"/chatgpt/backend-api\"\n")
		plan := inspect(t, path, testURL)
		if plan.Status != Unrouted || !plan.Has(DropLegacyURL) {
			t.Fatalf("plan = status %v, actions %v; want Unrouted with DropLegacyURL", plan.Status, plan.Actions)
		}
	})

	t.Run("Remove reports it instead of deleting it", func(t *testing.T) {
		content := "chatgpt_base_url = \"" + legacyURL + "\"\n"
		path := writeText(t, content)
		res, err := Remove(path)
		if err != nil {
			t.Fatal(err)
		}
		if res.Removed || len(res.Kept) != 1 || !strings.Contains(res.Kept[0], "chatgpt_base_url") {
			t.Errorf("Remove = %+v, want the old setting reported", res)
		}
		if got := readText(t, path); got != content {
			t.Errorf("Remove changed the file:\n%s", got)
		}
	})
}

func TestDefaultChatGPTBaseURLStays(t *testing.T) {
	cfg := "chatgpt_base_url = \"https://chatgpt.com/backend-api/\"\n"
	path := writeText(t, cfg)
	plan := inspect(t, path, testURL)
	if plan.Status != Unrouted || plan.Has(DropLegacyURL) {
		t.Fatalf("plan = status %v, actions %v; want Unrouted with AddRoute only", plan.Status, plan.Actions)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := readText(t, path), selectBlock+"\n"+cfg+"\n"+providerBlock(testURL); got != want {
		t.Errorf("config =\n%s\nwant\n%s", got, want)
	}
}

func TestInspectLeavesForeignConfigAlone(t *testing.T) {
	edited := func(from, to string) string {
		return strings.Replace(installedConfig(testURL), from, to, 1)
	}
	cases := []struct {
		name    string
		content string
		reason  string
	}{
		{"another model provider", "model_provider = \"azure\"\n", "another model provider"},
		{"the default provider named explicitly", "model_provider = \"openai\"\n", "another model provider"},
		{"unselected munch_chatgpt table", "[model_providers.munch_chatgpt]\nname = \"Mine\"\nbase_url = \"https://proxy.example.test/v1\"\n", "did not write"},
		{"selected table with extra settings", manualProvider(testURL) + "env_key = \"SOME_KEY\"\n", "differs from the documented setup"},
		{"selected table with another base_url", strings.Replace(manualProvider(testURL), testURL, "https://proxy.example.test/v1", 1), "differs from the documented setup"},
		{"invalid TOML", "model = \n", "not valid TOML"},
		{"model_providers that is not a table", "model_providers = 5\n", "not a table"},
		{"inline model_providers", "model_providers = { other = { name = \"Other\" } }\n", "without changing others"},
		{"custom chatgpt_base_url", "chatgpt_base_url = \"https://proxy.example.test/backend-api/\"\n", "custom backend"},
		{"plain HTTP chatgpt_base_url that is not munch's", "chatgpt_base_url = \"http://127.0.0.1:8787/backend-api/\"\n", "custom backend"},
		{"chatgpt_base_url that is not a string", "chatgpt_base_url = 5\n", "custom backend"},
		{"selected profile with its own provider", "profile = \"work\"\n\n[profiles.work]\nmodel_provider = \"azure\"\n", "profile"},
		{"edited provider block", edited(`name = "Munch"`, `name = "Mine"`), "edited by hand"},
		{"edited selection block", edited(`model_provider = "munch_chatgpt"`, `model_provider="munch_chatgpt"`), "edited by hand"},
		{"unpaired marker", "# >>> munch codex select >>>\nmodel = \"gpt-5\"\n", "do not pair up"},
		{"byte order mark", "\ufeffmodel = \"gpt-5\"\n", "byte order mark"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeText(t, tc.content)
			plan := inspect(t, path, testURL)
			if plan.Status != Blocked || !strings.Contains(plan.Reason, tc.reason) {
				t.Fatalf("plan = status %v, reason %q; want Blocked mentioning %q", plan.Status, plan.Reason, tc.reason)
			}
			if plan.NeedsWrite() {
				t.Errorf("a blocked plan must not make edits: %v", plan.Actions)
			}
			if strings.Contains(plan.Reason, "example.test") || strings.Contains(plan.Reason, testCap) {
				t.Errorf("the reason echoes the file: %q", plan.Reason)
			}
			if err := Apply(plan); err == nil {
				t.Error("Apply accepted a blocked plan")
			}
			if got := readText(t, path); got != tc.content {
				t.Errorf("a refused config was modified:\n%s", got)
			}
		})
	}
}

func TestInspectIgnoresInactiveProfiles(t *testing.T) {
	cfg := "[profiles.work]\nmodel_provider = \"azure\"\nmodel = \"gpt-5\"\n"
	path := writeText(t, cfg)
	install(t, path, testURL)
	if got, want := readText(t, path), selectBlock+"\n"+cfg+"\n"+providerBlock(testURL); got != want {
		t.Errorf("config =\n%s\nwant\n%s", got, want)
	}
}

func TestInspectRefusesFilesItCannotEditSafely(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		plan := inspect(t, t.TempDir(), testURL)
		if plan.Status != Blocked || !strings.Contains(plan.Reason, "not a regular file") {
			t.Errorf("plan = status %v, reason %q", plan.Status, plan.Reason)
		}
	})
	t.Run("read-only", func(t *testing.T) {
		path := writeText(t, typicalConfig)
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
		plan := inspect(t, path, testURL)
		if plan.Status != Blocked || !strings.Contains(plan.Reason, "read-only") {
			t.Errorf("plan = status %v, reason %q", plan.Status, plan.Reason)
		}
	})
	t.Run("too large", func(t *testing.T) {
		path := writeText(t, strings.Repeat("# padding padding padding\n", 50000))
		plan := inspect(t, path, testURL)
		if plan.Status != Blocked || !strings.Contains(plan.Reason, "larger than 1 MiB") {
			t.Errorf("plan = status %v, reason %q", plan.Status, plan.Reason)
		}
	})
}

func TestSymlinkedConfigStaysASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "codex.toml")
	link := filepath.Join(dir, "home", "config.toml")
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte(typicalConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	install(t, link, testURL)
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("install replaced the symlink (err=%v)", err)
	}
	if got, want := readText(t, target), installedConfig(testURL); got != want {
		t.Errorf("the file behind the symlink =\n%s\nwant\n%s", got, want)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the file behind the symlink must be 0600 (mode=%v)", info.Mode().Perm())
	}

	if res, err := Remove(link); err != nil || !res.Removed {
		t.Fatalf("Remove = %+v, err=%v", res, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Remove replaced the symlink (err=%v)", err)
	}
	if got := readText(t, target); got != typicalConfig {
		t.Errorf("the file behind the symlink after Remove =\n%s", got)
	}
}

func TestApplyRefusesToOverwriteAConcurrentEdit(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		path := writeText(t, typicalConfig)
		plan := inspect(t, path, testURL)
		changed := typicalConfig + "extra = 1\n"
		if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Apply(plan); !errors.Is(err, ErrChanged) {
			t.Fatalf("Apply = %v, want ErrChanged", err)
		}
		if got := readText(t, path); got != changed {
			t.Errorf("Apply clobbered the concurrent edit:\n%s", got)
		}
		if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
			t.Errorf("a temporary file was left behind: %v (err=%v)", entries, err)
		}
	})

	t.Run("file created after Inspect", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		plan := inspect(t, path, testURL)
		if err := os.WriteFile(path, []byte("model = \"gpt-5\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Apply(plan); !errors.Is(err, ErrChanged) {
			t.Fatalf("Apply = %v, want ErrChanged", err)
		}
		if got := readText(t, path); got != "model = \"gpt-5\"\n" {
			t.Errorf("Apply clobbered the new file:\n%s", got)
		}
	})
}

func TestCRLFConfigKeepsItsLineEndings(t *testing.T) {
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	path := writeText(t, crlf(typicalConfig))
	install(t, path, testURL)
	if got, want := readText(t, path), crlf(installedConfig(testURL)); got != want {
		t.Errorf("config =\n%q\nwant\n%q", got, want)
	}
	if res, err := Remove(path); err != nil || !res.Removed {
		t.Fatalf("Remove = %+v, err=%v", res, err)
	}
	if got := readText(t, path); got != crlf(typicalConfig) {
		t.Errorf("config after Remove = %q, want the original back", got)
	}
}

func TestConfigWithoutATrailingNewline(t *testing.T) {
	path := writeText(t, "model = \"gpt-5\"")
	install(t, path, testURL)
	if got, want := readText(t, path), selectBlock+"\nmodel = \"gpt-5\"\n\n"+providerBlock(testURL); got != want {
		t.Errorf("config =\n%s\nwant\n%s", got, want)
	}
	if res, err := Remove(path); err != nil || !res.Removed {
		t.Fatalf("Remove = %+v, err=%v", res, err)
	}
	if got := readText(t, path); got != "model = \"gpt-5\"\n" {
		t.Errorf("config after Remove = %q", got)
	}
}

func TestInspectRejectsAnArbitraryBaseURL(t *testing.T) {
	path := writeText(t, typicalConfig)
	for _, bad := range []string{
		"",
		"https://proxy.example.test/v1",
		"http://203.0.113.9:8787/_tm/" + testCap + "/chatgpt/backend-api/codex",
		"http://127.0.0.1:8787/chatgpt/backend-api/codex",
		strings.TrimSuffix(testURL, "/codex"),
	} {
		if _, err := Inspect(path, bad); err == nil {
			t.Errorf("Inspect accepted %q as a munch route", bad)
		}
	}
}

func TestRemoveKeepsAnEditedProviderAndReportsIt(t *testing.T) {
	editedProvider := strings.Replace(providerBlock(testURL), `name = "Munch"`, `name = "Mine"`, 1)
	path := writeText(t, selectBlock+"\n"+typicalConfig+"\n"+editedProvider)

	res, err := Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	// The selection is munch's and goes, so Codex falls back to its default
	// provider; the edited provider is the user's and stays, unused.
	if !res.Removed || len(res.Kept) != 2 ||
		!strings.Contains(res.Kept[0], "edited by hand") || !strings.Contains(res.Kept[1], "[model_providers.munch_chatgpt]") {
		t.Errorf("Remove = %+v, want the selection removed and the edited provider reported", res)
	}
	if got, want := readText(t, path), typicalConfig+"\n"+editedProvider; got != want {
		t.Errorf("config after Remove =\n%s\nwant\n%s", got, want)
	}
}

func TestRemoveKeepsTheProviderWhileASelectionSurvives(t *testing.T) {
	// Each edit changes only the selection block's text; the config still
	// selects munch_chatgpt. Deleting the provider would leave Codex pointing
	// at a provider that does not exist.
	cases := []struct {
		name  string
		block string
	}{
		{"selection line retyped", strings.Replace(selectBlock, `model_provider = "munch_chatgpt"`, `model_provider="munch_chatgpt" # mine`, 1)},
		{"notice comment changed", strings.Replace(selectBlock, "# Managed by munch install; removed by munch uninstall.", "# my own note", 1)},
		{"comment added inside", strings.Replace(selectBlock, "# <<< munch codex select <<<", "# and this too\n# <<< munch codex select <<<", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.block + "\n" + typicalConfig + "\n" + providerBlock(testURL)
			path := writeText(t, content)

			res, err := Remove(path)
			if err != nil {
				t.Fatal(err)
			}
			if res.Removed {
				t.Error("Remove deleted something although the selection survives")
			}
			if got := readText(t, path); got != content {
				t.Errorf("config after Remove =\n%s\nwant it untouched", got)
			}
			kept := strings.Join(res.Kept, "|")
			for _, want := range []string{"edited by hand", `model_provider = "munch_chatgpt"`, "[model_providers.munch_chatgpt]"} {
				if !strings.Contains(kept, want) {
					t.Errorf("Kept = %v, missing %q", res.Kept, want)
				}
			}

			// The config still selects a provider that exists.
			doc, err := parse(readText(t, path))
			if err != nil {
				t.Fatal(err)
			}
			providers, _ := doc["model_providers"].(map[string]any)
			if doc["model_provider"] != "munch_chatgpt" || providers["munch_chatgpt"] == nil {
				t.Errorf("config no longer pairs the selection with its provider: %v", doc)
			}
		})
	}
}

func TestRemoveDropsTheProviderOnceNothingSelectsIt(t *testing.T) {
	// The user pointed munch's selection block at another provider.
	changed := strings.Replace(selectBlock, `"munch_chatgpt"`, `"other"`, 1)
	path := writeText(t, changed+"\n"+typicalConfig+"\n"+providerBlock(testURL))

	res, err := Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removed || len(res.Kept) != 1 || !strings.Contains(res.Kept[0], "edited by hand") {
		t.Errorf("Remove = %+v, want only the provider removed and the edited block reported", res)
	}
	if got, want := readText(t, path), changed+"\n"+typicalConfig; got != want {
		t.Errorf("config after Remove =\n%s\nwant\n%s", got, want)
	}
}

func TestRemoveWithoutMunchSettingsChangesNothing(t *testing.T) {
	path := writeText(t, typicalConfig)
	res, err := Remove(path)
	if err != nil || res.Removed || len(res.Kept) != 0 {
		t.Errorf("Remove = %+v, err=%v, want nothing to do", res, err)
	}
	if got := readText(t, path); got != typicalConfig {
		t.Errorf("Remove changed a config without munch settings:\n%s", got)
	}

	res, err = Remove(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil || res.Removed || len(res.Kept) != 0 {
		t.Errorf("Remove on a missing file = %+v, err=%v, want nothing to do", res, err)
	}
}

func TestRemoveRefusesWhatItCannotReadSafely(t *testing.T) {
	t.Run("invalid TOML with munch's markers", func(t *testing.T) {
		path := writeText(t, "# >>> munch codex select >>>\nmodel = \n")
		if _, err := Remove(path); err == nil || !strings.Contains(err.Error(), "not valid TOML") {
			t.Errorf("Remove error = %v, want it to say the file is not valid TOML", err)
		}
	})
	t.Run("invalid TOML without munch settings", func(t *testing.T) {
		path := writeText(t, "model = \n")
		if res, err := Remove(path); err != nil || res.Removed || len(res.Kept) != 0 {
			t.Errorf("Remove = %+v, err=%v, want nothing to do", res, err)
		}
	})
	t.Run("unpaired markers", func(t *testing.T) {
		content := "# >>> munch codex provider >>>\nmodel = \"gpt-5\"\n"
		path := writeText(t, content)
		if _, err := Remove(path); err == nil || !strings.Contains(err.Error(), "do not pair up") {
			t.Errorf("Remove error = %v, want it to say the markers do not pair up", err)
		}
		if got := readText(t, path); got != content {
			t.Errorf("Remove changed the file:\n%s", got)
		}
	})
	t.Run("read-only", func(t *testing.T) {
		path := writeText(t, typicalConfig)
		install(t, path, testURL)
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
		before := readText(t, path)
		if _, err := Remove(path); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("Remove error = %v, want it to say the file is read-only", err)
		}
		if got := readText(t, path); got != before {
			t.Errorf("Remove changed a read-only file:\n%s", got)
		}
	})
}

// A second parser, if the machine has one, confirms the TOML munch writes.
func TestEditedConfigsParseWithAnIndependentParser(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	if err := exec.Command(python, "-c", "import tomllib").Run(); err != nil {
		t.Skip("python3 has no tomllib")
	}
	cases := map[string]string{
		"missing":   "",
		"typical":   typicalConfig,
		"crlf":      strings.ReplaceAll(typicalConfig, "\n", "\r\n"),
		"old setup": "model = \"gpt-5\"\nchatgpt_base_url = \"" + legacyURL + "\"\n\n[projects.\"/work/app\"]\ntrust_level = \"trusted\"\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeText(t, content)
			install(t, path, testURL)

			cmd := exec.Command(python, "-c", "import json, sys, tomllib; print(json.dumps(tomllib.loads(sys.stdin.read())))")
			cmd.Stdin = strings.NewReader(readText(t, path))
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("python could not parse the edited config: %v\n%s", err, readText(t, path))
			}
			var doc map[string]any
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			providers, _ := doc["model_providers"].(map[string]any)
			provider, _ := providers["munch_chatgpt"].(map[string]any)
			if doc["model_provider"] != "munch_chatgpt" || provider["base_url"] != testURL ||
				provider["name"] != "Munch" || provider["wire_api"] != "responses" || provider["requires_openai_auth"] != true {
				t.Errorf("python read the provider as %v", doc)
			}
			if _, ok := doc["chatgpt_base_url"]; ok {
				t.Error("the old chatgpt_base_url survived")
			}
			if content != "" && doc["model"] != "gpt-5" {
				t.Errorf("python read model as %v", doc["model"])
			}
		})
	}
}

func TestValuesEqual(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"same nested values", map[string]any{"a": []any{int64(1), "x"}}, map[string]any{"a": []any{int64(1), "x"}}, true},
		{"missing key", map[string]any{"a": int64(1)}, map[string]any{}, false},
		{"different value", map[string]any{"a": int64(1)}, map[string]any{"a": int64(2)}, false},
		{"different array length", []any{int64(1)}, []any{int64(1), int64(2)}, false},
		{"different types", int64(1), "1", false},
		{"NaN equals NaN", math.NaN(), math.NaN(), true},
		{"same instant in another zone", when, when.In(time.FixedZone("x", 3600)), true},
		{"different instants", when, when.Add(time.Second), false},
	}
	for _, tc := range cases {
		if got := valuesEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: valuesEqual = %v, want %v", tc.name, got, tc.want)
		}
	}
}

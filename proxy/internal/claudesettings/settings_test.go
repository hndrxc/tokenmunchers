package claudesettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/proxy"
)

func testEndpoint(port string) string {
	return "http://127.0.0.1:" + port + "/_tm/" + strings.Repeat("a", 64) + proxy.ClaudeLogsPath
}

var otherEndpoint = testEndpoint("9002")

// writeSettings writes content at path, creating parent directories.
func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// readEnv decodes path and returns its env object as a plain map.
func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(readText(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Env
}

// install inspects and applies, failing the test when the settings are refused.
func install(t *testing.T, path, endpoint string) *Plan {
	t.Helper()
	plan, err := Inspect(path, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status == Blocked {
		t.Fatalf("settings blocked: %s", plan.Reason)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestDirAndEnv(t *testing.T) {
	getenv := func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return filepath.Join("cfg", "claude")
		}
		return ""
	}
	if got := Dir(getenv, "home"); got != filepath.Join("cfg", "claude") {
		t.Errorf("Dir with $CLAUDE_CONFIG_DIR = %q", got)
	}
	empty := func(string) string { return "" }
	if got := Dir(empty, "home"); got != filepath.Join("home", ".claude") {
		t.Errorf("Dir without $CLAUDE_CONFIG_DIR = %q", got)
	}
	if got := Path("/x"); got != filepath.Join("/x", "settings.json") {
		t.Errorf("Path = %q", got)
	}

	env := Env(testEndpoint("8787"))
	if len(env) != 19 {
		t.Errorf("Env has %d keys, want 19", len(env))
	}
	if env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != testEndpoint("8787") {
		t.Errorf("Env endpoint = %q", env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"])
	}
}

func TestInstallCreatesAPrivateSettingsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "claude", "nested")
	path := Path(dir)
	plan := install(t, path, testEndpoint("8787"))

	if !plan.NeedsWrite() {
		t.Error("an unconfigured settings file needs a write")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("settings file mode = %o, want 600", info.Mode().Perm())
		}
		if mode, err := os.Stat(dir); err != nil || mode.Mode().Perm() != 0o700 {
			t.Errorf("settings dir mode = %v, err=%v; want 700", mode, err)
		}
	}
	if !strings.HasSuffix(readText(t, path), "\n") {
		t.Error("settings file must end with a newline")
	}
	env := readEnv(t, path)
	if len(env) != 19 {
		t.Errorf("env has %d keys, want 19", len(env))
	}
	for name, want := range Env(testEndpoint("8787")) {
		if env[name] != want {
			t.Errorf("env[%s] = %q, want %q", name, env[name], want)
		}
	}
}

func TestApplyKeepsOtherKeysAndOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"z":1,"env":{"KEEP":"x"},"a":"outer"}`)
	plan := install(t, path, testEndpoint("8787"))

	text := readText(t, path)
	z := strings.Index(text, `"z"`)
	env := strings.Index(text, `"env"`)
	a := strings.Index(text, `"a"`)
	if !(z < env && env < a) {
		t.Errorf("top-level key order changed:\n%s", text)
	}
	keep := strings.Index(text, `"KEEP"`)
	first := strings.Index(text, `"CLAUDE_CODE_ENABLE_TELEMETRY"`)
	if !(keep < first) {
		t.Errorf("munch keys must be appended after existing env keys:\n%s", text)
	}
	if !strings.Contains(text, `"a": "outer"`) {
		t.Errorf("top-level value must be kept:\n%s", text)
	}
	_ = plan
}

func TestCurrentNeedsNoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"env":`+jsonEnv(testEndpoint("8787"))+`}`)
	plan, err := Inspect(path, testEndpoint("8787"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != Current {
		t.Fatalf("status = %v, want Current", plan.Status)
	}
	if plan.NeedsWrite() {
		t.Error("a current settings file needs no write")
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
}

func jsonEnv(endpoint string) string {
	var b strings.Builder
	b.WriteString("{")
	vals := Env(endpoint)
	first := true
	for _, k := range envNames {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(quoteString(k))
		b.WriteString(":")
		b.WriteString(quoteString(vals[k]))
	}
	b.WriteString("}")
	return b.String()
}

func TestStaleManagedSetIsRepointed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"env":`+jsonEnv(otherEndpoint)+`}`)
	plan, err := Inspect(path, testEndpoint("8787"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != Stale {
		t.Fatalf("status = %v, want Stale", plan.Status)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	env := readEnv(t, path)
	if env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != testEndpoint("8787") {
		t.Errorf("endpoint not repointed: %q", env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"])
	}
	for name, want := range Env(testEndpoint("8787")) {
		if env[name] != want {
			t.Errorf("env[%s] = %q, want %q", name, env[name], want)
		}
	}
}

func TestBlockedCases(t *testing.T) {
	tooBig := strings.Repeat(" ", 1<<20+1)
	cases := []struct {
		name   string
		setup  func(t *testing.T) string
		reason string
	}{
		{"not a regular file", func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), "settings.json")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "not a regular file"},
		{"larger than 1 MiB", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":{"pad":"`+tooBig+`"}}`)
			return path
		}, "larger than 1 MiB"},
		{"invalid JSON", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":`)
			return path
		}, "not valid JSON"},
		{"top level is not an object", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `[1,2]`)
			return path
		}, "top level is not an object"},
		{"duplicate keys", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":{},"env":{}}`)
			return path
		}, "duplicate keys"},
		{"env is not an object", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":"x"}`)
			return path
		}, "env is not an object"},
		{"env value is not a string", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":{"KEEP":1}}`)
			return path
		}, "not a string"},
		{"otelHeadersHelper present", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":{},"otelHeadersHelper":{}}`)
			return path
		}, "otelHeadersHelper"},
		{"foreign telemetry key", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":{"OTEL_EXPORTER_OTLP_SPAN_ENDPOINT":"https://example.com","OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":"`+testEndpoint("8787")+`"}}`)
			return path
		}, "other telemetry settings"},
		{"munch-managed key without endpoint", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":"1"}}`)
			return path
		}, "other telemetry settings"},
		{"non-munch endpoint", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeSettings(t, path, `{"env":{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":"https://example.com/v1/logs"}}`)
			return path
		}, "exports telemetry somewhere else"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			plan, err := Inspect(path, testEndpoint("8787"))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != Blocked {
				t.Fatalf("status = %v, want Blocked", plan.Status)
			}
			if !strings.Contains(plan.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to contain %q", plan.Reason, tc.reason)
			}
			if plan.Reason != "" && strings.Contains(plan.Reason, testEndpoint("8787")) {
				t.Errorf("reason echoes the endpoint: %q", plan.Reason)
			}
			if err := Apply(plan); err == nil {
				t.Error("Apply of a blocked plan must fail")
			}
		})
	}
}

func TestRemoveOnlyMunchKeysAndReportsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"env":{"KEEP":"x","OTEL_LOG_TOOL_DETAILS":"1","OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":"`+testEndpoint("8787")+`","OTEL_LOG_TOOL_CONTENT":"0","OTEL_LOG_RAW_API_BODIES":"0"}}`)

	res, err := Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removed {
		t.Error("Remove should have removed munch keys")
	}
	if !reflect.DeepEqual(res.Kept, []string{"OTEL_LOG_TOOL_DETAILS"}) {
		t.Errorf("Kept = %v, want [OTEL_LOG_TOOL_DETAILS]", res.Kept)
	}
	env := readEnv(t, path)
	if env["OTEL_LOG_TOOL_CONTENT"] != "" || env["OTEL_LOG_RAW_API_BODIES"] != "" {
		t.Errorf("munch keys were not all removed: %v", env)
	}
	if env["KEEP"] != "x" {
		t.Errorf("foreign env keys must stay: %v", env)
	}
}

func TestRemoveDropsEnvWhenItEndsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"env":`+jsonEnv(testEndpoint("8787"))+`}`)
	res, err := Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removed {
		t.Error("Remove should report removal")
	}
	if text := readText(t, path); !strings.Contains(text, `{}`) {
		t.Errorf("empty env should be dropped, got:\n%s", text)
	}
}

func TestRemoveForeignEndpointIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"env":{"OTEL_LOG_TOOL_CONTENT":"0","OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":"https://example.com/v1/logs"}}`)
	res, err := Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed || len(res.Kept) != 0 {
		t.Errorf("Remove touched a foreign telemetry setup: %+v", res)
	}
	if got := readText(t, path); !strings.Contains(got, "example.com") {
		t.Errorf("file changed:\n%s", got)
	}
}

func TestRemoveMissingOrInvalidFilesAreNoOps(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "settings.json")
	if res, err := Remove(missing); err != nil || res.Removed {
		t.Errorf("Remove of a missing file = %+v, %v", res, err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"env":`)
	if res, err := Remove(path); err != nil || res.Removed {
		t.Errorf("Remove of an invalid file = %+v, %v", res, err)
	}
	if got := readText(t, path); got != `{"env":` {
		t.Errorf("invalid file was modified: %q", got)
	}
}

func TestApplyRefusesToOverwriteAConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"env":{"KEEP":"x"}}`)
	plan, err := Inspect(path, testEndpoint("8787"))
	if err != nil {
		t.Fatal(err)
	}
	writeSettings(t, path, `{"env":{"KEEP":"hand-edited"}}`)
	if err := Apply(plan); err != ErrChanged {
		t.Fatalf("Apply error = %v, want ErrChanged", err)
	}
	if got := readText(t, path); !strings.Contains(got, "hand-edited") {
		t.Errorf("the concurrent edit was overwritten:\n%s", got)
	}
}

func TestCRLFConfigKeepsItsLineEndings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, "{\r\n  \"env\": {\r\n    \"KEEP\": \"x\"\r\n  }\r\n}\r\n")
	install(t, path, testEndpoint("8787"))
	text := readText(t, path)
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Errorf("CRLF settings must keep CRLF line endings:\n%q", text)
	}
	env := readEnv(t, path)
	if len(env) != 20 { // KEEP + 19 munch keys
		t.Errorf("env has %d keys, want 20", len(env))
	}
}

func TestSymlinkedSettingsStaysASymlink(t *testing.T) {
	if testing.Short() {
		t.Skip("symlinks in short mode")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	writeSettings(t, target, `{"env":{"KEEP":"x"}}`)
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	install(t, link, testEndpoint("8787"))
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("settings symlink was replaced by a regular file")
	}
	if env := readEnv(t, link); len(env) != 20 {
		t.Errorf("target env has %d keys, want 20", len(env))
	}
}

func TestInspectRejectsAnArbitraryEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := Inspect(path, "https://example.com/v1/logs"); err == nil {
		t.Error("Inspect accepted a non-Munch endpoint")
	}
	if _, err := Inspect(path, "http://127.0.0.1:8787/_tm/123/__desktop/v1/logs"); err == nil {
		t.Error("Inspect accepted a short capability")
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/desktop"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/proxy"
)

func TestDesktopCLIOptInAndManualClaudeEnv(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	for _, key := range []string{"TOKENMUNCHERS_KEY", "TOKENMUNCHERS_API_KEY", "TOKENMUNCHERS_URL", "TMPROXY_LISTEN"} {
		t.Setenv(key, "")
	}
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("CODEX_HOME", codexDir)
	settings := map[string]string{
		filepath.Join(claudeDir, "settings.json"): `{"env":{"KEEP":"unchanged"}}`,
		filepath.Join(codexDir, "config.toml"):    "model = \"personal-choice\"\n",
	}
	for path, data := range settings {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, stderr bytes.Buffer
	a := &App{Stdout: &out, Stderr: &stderr}
	run := func(want int, args ...string) string {
		t.Helper()
		out.Reset()
		stderr.Reset()
		argv := []string{"desktop", "--config-dir", paths.ConfigDir, "--state-dir", paths.StateDir}
		if code := a.Run(append(argv, args...)); code != want {
			t.Fatalf("desktop %v exited %d: %s", args, code, stderr.String())
		}
		return out.String()
	}
	if got := run(0); !strings.Contains(got, "Claude Desktop: off") || !strings.Contains(got, "Codex Desktop:  off") {
		t.Fatalf("unexpected default status: %q", got)
	}
	run(1, "claude", "on")
	cfg := config.Config{APIKey: "tm_" + strings.Repeat("A", 43)}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	key, err := localauth.LoadOrCreate(paths.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	run(1, "claude-env")
	run(0, "claude", "on")
	first, err := config.LoadFile(paths)
	if err != nil || first.DesktopClaudeSince.IsZero() || !first.DesktopCodexSince.IsZero() {
		t.Fatalf("unexpected opt-in state: %v", err)
	}
	run(0, "claude", "on")
	again, err := config.LoadFile(paths)
	if err != nil || !again.DesktopClaudeSince.Equal(first.DesktopClaudeSince) {
		t.Fatalf("idempotent opt-in reset the activation time: %v", err)
	}
	if got := run(1, "--listen", "127.0.0.1:0", "claude-env"); got != "" {
		t.Fatal("claude-env printed settings for an ephemeral port")
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(run(0, "--listen", "127.0.0.1:9876", "claude-env")), &env); err != nil {
		t.Fatal(err)
	}
	if env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != "http://127.0.0.1:9876"+key.RoutePrefix()+proxy.ClaudeLogsPath || len(env) != 19 {
		t.Fatal("incomplete or incorrectly scoped Claude telemetry settings")
	}
	for name, value := range env {
		if strings.Contains(name, "BASE_URL") || strings.Contains(value, cfg.APIKey) || (strings.HasPrefix(name, "OTEL_LOG_") && value != "0") {
			t.Fatalf("unsafe Claude telemetry setting %s", name)
		}
	}
	run(0, "codex", "on")
	run(0, "claude", "off")
	final, err := config.LoadFile(paths)
	if err != nil || !final.DesktopClaudeSince.IsZero() || final.DesktopCodexSince.IsZero() {
		t.Fatalf("sources were not independently configured: %v", err)
	}
	run(1, "claude-env")
	if _, err := os.Stat(desktopDir(paths)); !os.IsNotExist(err) {
		t.Fatalf("desktop configuration touched collector state: %v", err)
	}
	for path, want := range settings {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("provider settings modified: %v", err)
		}
	}
}

func TestDesktopLogsRequireOptInAndLocalCapability(t *testing.T) {
	key, err := localauth.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := key.RoutePrefix() + proxy.ClaudeLogsPath
	for _, tc := range []struct {
		name, path, host, header string
		enabled                  bool
		status                   int
	}{
		{"disabled", endpoint, "127.0.0.1", "", false, http.StatusNotFound},
		{"accepted", endpoint, "127.0.0.1", "", true, http.StatusNoContent},
		{"missing capability", proxy.ClaudeLogsPath, "127.0.0.1", "", true, http.StatusNotFound},
		{"wrong capability", "/wrong" + proxy.ClaudeLogsPath, "127.0.0.1", "", true, http.StatusNotFound},
		{"browser origin", endpoint, "127.0.0.1", "Origin", true, http.StatusForbidden},
		{"browser fetch metadata", endpoint, "127.0.0.1", "Sec-Fetch-Site", true, http.StatusForbidden},
		{"rebinding host", endpoint, "example.invalid", "", true, http.StatusForbidden},
		{"reserved sibling", endpoint + "/other", "127.0.0.1", "", true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			opts := proxy.Options{LocalKey: key}
			if tc.enabled {
				opts.ClaudeLogs = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.WriteHeader(http.StatusNoContent)
				})
			}
			srv, err := proxy.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "http://"+tc.host+tc.path, nil)
			if tc.header != "" {
				req.Header.Set(tc.header, "cross-site")
			}
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			wantCalls := 0
			if tc.status == http.StatusNoContent {
				wantCalls = 1
			}
			if w.Code != tc.status || calls != wantCalls {
				t.Fatalf("got HTTP %d with %d handler calls; want HTTP %d", w.Code, calls, tc.status)
			}
		})
	}
}

func TestDesktopDaemonDoesNotOpenCollectorOnFailedBind(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	for _, key := range []string{"TOKENMUNCHERS_KEY", "TOKENMUNCHERS_API_KEY", "TOKENMUNCHERS_URL", "TMPROXY_LISTEN"} {
		t.Setenv(key, "")
	}
	cfg := config.Config{APIKey: "tm_" + strings.Repeat("A", 43), DesktopClaudeSince: time.Now().UTC()}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var stderr bytes.Buffer
	a := &App{Stderr: &stderr}
	err = a.cmdRun([]string{"--config-dir", paths.ConfigDir, "--state-dir", paths.StateDir, "--listen", listener.Addr().String()})
	if err == nil || !strings.HasPrefix(err.Error(), "listen ") {
		t.Fatalf("expected bind failure, got %v", err)
	}
	if _, err := os.Stat(desktopDir(paths)); !os.IsNotExist(err) {
		t.Fatalf("failed bind touched collector state: %v", err)
	}
}

func TestOfflinePausePersistsBeforeFirstDesktopBind(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	for _, key := range []string{"TOKENMUNCHERS_KEY", "TOKENMUNCHERS_API_KEY", "TOKENMUNCHERS_URL", "TMPROXY_LISTEN"} {
		t.Setenv(key, "")
	}
	cfg := config.Config{DesktopCodexSince: time.Unix(1, 0).UTC()}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := &App{Stdout: &out, Stderr: &stderr}
	args := []string{"--config-dir", paths.ConfigDir, "--state-dir", paths.StateDir}
	if err := a.cmdPause(args, true); err != nil {
		t.Fatal(err)
	}
	duringPause := time.Now().UTC()
	if err := a.cmdPause(args, false); err != nil {
		t.Fatal(err)
	}
	disk, err := config.LoadFile(paths)
	if err != nil || disk.Paused || !disk.DesktopCodexSince.Equal(cfg.DesktopCodexSince) {
		t.Fatalf("unexpected config after resume: %v", err)
	}
	ledger, err := desktop.Open(desktop.Options{
		Dir: desktopDir(paths), IngestURL: config.DefaultIngestURL,
		APIKey: "tm_" + strings.Repeat("A", 43), CodexSince: disk.DesktopCodexSince,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if err := ledger.Record(desktop.Observation{
		Harness: desktop.Codex, CanonicalID: "resp_paused", IdentityStrength: "response",
		Provider: "openai", Model: "unknown", Timestamp: duringPause, InputTokens: 1,
	}); err != nil || ledger.Pending() != 0 {
		t.Fatalf("a delayed paused-period response was not excluded: %v", err)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAPIKey(t *testing.T) {
	valid := "tm_" + strings.Repeat("a", 43)
	if err := ValidateAPIKey(valid); err != nil {
		t.Fatalf("expected valid key, got %v", err)
	}
	for _, bad := range []string{"", "tm_short", "sk_" + strings.Repeat("a", 43), "tm_" + strings.Repeat("!", 43)} {
		if err := ValidateAPIKey(bad); err == nil {
			t.Fatalf("expected error for invalid key %q", bad)
		}
	}
}

func TestSaveLoadAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{ConfigDir: filepath.Join(dir, "cfg"), StateDir: filepath.Join(dir, "state")}

	disk := Config{
		ListenAddr: "127.0.0.1:9999",
		APIKey:     "tm_" + strings.Repeat("B", 43),
		Routes: map[string]Route{
			"vllm": {Upstream: "http://127.0.0.1:8000", Provider: "vllm", Format: FormatOpenAI, Local: true},
		},
	}
	if err := Save(paths, disk); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	info, err := os.Stat(paths.ConfigFile())
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", perm)
	}

	envKey := "tm_" + strings.Repeat("C", 43)
	t.Setenv("TOKENMUNCHERS_KEY", envKey)
	t.Setenv("TOKENMUNCHERS_URL", "https://example.test/functions/v1/ingest")

	loaded, err := Load(paths)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.APIKey != envKey {
		t.Fatalf("expected env key override, got %q", loaded.APIKey)
	}
	if loaded.IngestURL != "https://example.test/functions/v1/ingest" {
		t.Fatalf("expected env URL override, got %q", loaded.IngestURL)
	}
	if loaded.ListenAddr != "127.0.0.1:9999" {
		t.Fatalf("expected listen addr 127.0.0.1:9999, got %q", loaded.ListenAddr)
	}
	if _, ok := loaded.Routes["anthropic"]; !ok {
		t.Fatal("expected builtin anthropic route to be present")
	}
	if r, ok := loaded.Routes["vllm"]; !ok || r.Upstream != "http://127.0.0.1:8000" || !r.Local {
		t.Fatalf("expected custom vllm route, got %+v", r)
	}
}

func TestNormalizeListenAddr(t *testing.T) {
	ok := map[string]string{"": DefaultListen, "localhost:80": "127.0.0.1:80", "127.0.0.1:0": "127.0.0.1:0", "[::1]:8080": "[::1]:8080"}
	for in, want := range ok {
		got, err := NormalizeListenAddr(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %q, %v want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"0.0.0.0:80", "example.com:80", "127.0.0.1", "127.0.0.1:65536", "127.0.0.1:+1", "127.0.0.1:x", ":80"} {
		if _, err := NormalizeListenAddr(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestValidateRoute(t *testing.T) {
	for _, u := range []string{"https://api.example.com/v1", "http://127.0.0.1:8000", "http://[::1]:8000"} {
		if err := ValidateRoute("x", Route{Upstream: u}); err != nil {
			t.Fatalf("%q: %v", u, err)
		}
	}
	for _, u := range []string{"", "http://example.com", "http://localhost:1", "https://u:p@example.com", "https://example.com?a=1", "https://example.com#f", "ftp://example.com", "https://"} {
		err := ValidateRoute("x", Route{Upstream: u})
		if err == nil {
			t.Fatalf("expected error for %q", u)
		}
		if u != "" && strings.Contains(err.Error(), u) {
			t.Fatalf("error leaks URL: %v", err)
		}
	}
}

func TestLoadRejectsBadRoutes(t *testing.T) {
	for _, routes := range []map[string]Route{
		{"anthropic": {Upstream: "http://evil.example"}},
		{"a": {Upstream: "https://a.example"}, "A/": {Upstream: "https://b.example"}},
		{"x": {}},
	} {
		dir := t.TempDir()
		paths := Paths{ConfigDir: dir, StateDir: dir}
		if err := Save(paths, Config{Routes: routes}); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(paths); err == nil {
			t.Fatalf("expected error for %+v", routes)
		}
	}
}

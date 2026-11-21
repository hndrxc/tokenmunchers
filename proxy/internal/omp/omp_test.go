package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// realistic shape of `omp plugin list --json`
const listWithPlugin = `{
  "npm": [
    {
      "name": "omp-lemmeaskchat",
      "version": "0.3.1",
      "path": "/home/u/.omp/plugins/node_modules/omp-lemmeaskchat",
      "manifest": {"extensions": ["src/index.ts"], "version": "0.3.1"},
      "enabledFeatures": null,
      "enabled": true
    },
    {
      "name": "omp-tokenmunchers",
      "version": "0.2.0",
      "path": "/home/u/.omp/plugins/node_modules/omp-tokenmunchers",
      "manifest": {"extensions": ["index.ts"], "version": "0.2.0"},
      "enabledFeatures": null,
      "enabled": true
    }
  ],
  "marketplace": []
}`

const listWithoutPlugin = `{
  "npm": [
    {"name": "omp-tokenmunchers-extras", "version": "1.0.0", "path": "/x", "manifest": {"version": "1.0.0"}, "enabledFeatures": null, "enabled": true}
  ],
  "marketplace": [{"id": "omp-tokenmunchers@market"}]
}`

const listEmpty = `{"npm": [], "marketplace": []}`

func fakeOmp(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fakes need a POSIX shell")
	}
	p := filepath.Join(t.TempDir(), "omp")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPluginInstalled(t *testing.T) {
	cases := []struct {
		name, out string
		want      bool
	}{
		{"present", listWithPlugin, true},
		{"similar name only", listWithoutPlugin, false},
		{"empty", listEmpty, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outFile := filepath.Join(t.TempDir(), "out.json")
			argsFile := filepath.Join(t.TempDir(), "args")
			if err := os.WriteFile(outFile, []byte(tc.out), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := fakeOmp(t, `printf '%s\n' "$@" > '`+argsFile+`'
echo "warning noise" >&2
cat '`+outFile+"'\n")
			got, err := PluginInstalled(context.Background(), bin)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			args, _ := os.ReadFile(argsFile)
			if string(args) != "plugin\nlist\n--json\n" {
				t.Fatalf("argv = %q", args)
			}
		})
	}
}

func TestPluginInstalledErrors(t *testing.T) {
	t.Run("non-zero exit includes stderr", func(t *testing.T) {
		bin := fakeOmp(t, "echo 'boom: broken' >&2\nexit 3\n")
		ok, err := PluginInstalled(context.Background(), bin)
		if err == nil || ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if !strings.Contains(err.Error(), "boom: broken") {
			t.Fatalf("stderr missing: %v", err)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		bin := fakeOmp(t, "echo not-json\n")
		if _, err := PluginInstalled(context.Background(), bin); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		if _, err := PluginInstalled(context.Background(), filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("context cancelled", func(t *testing.T) {
		bin := fakeOmp(t, "echo '{}'\n")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := PluginInstalled(ctx, bin); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestInstallPlugin(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := fakeOmp(t, `printf '%s\n' "$@" > '`+argsFile+`'
echo installing
echo warn >&2
`)
	var out, errb bytes.Buffer
	if err := InstallPlugin(context.Background(), bin, &out, &errb); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if string(args) != "plugin\ninstall\nomp-tokenmunchers\n" {
		t.Fatalf("argv = %q", args)
	}
	if out.String() != "installing\n" || errb.String() != "warn\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errb.String())
	}
}

func TestInstallPluginFailureAndStdin(t *testing.T) {
	// stdin must be empty (/dev/null), not inherited: `cat` returns immediately.
	bin := fakeOmp(t, "cat >/dev/null\necho nope >&2\nexit 1\n")
	var errb bytes.Buffer
	if err := InstallPlugin(context.Background(), bin, new(bytes.Buffer), &errb); err == nil {
		t.Fatal("expected error")
	}
	if errb.String() != "nope\n" {
		t.Fatalf("stderr=%q", errb.String())
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

func TestSeedKeyNewFile(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "agent")
	seeded, err := SeedKey(agent, "tm_secret", "https://example.test/functions/v1/ingest?a=1&b=2")
	if err != nil || !seeded {
		t.Fatalf("seeded=%v err=%v", seeded, err)
	}
	dir := filepath.Join(agent, "tokenmunchers")
	path := filepath.Join(dir, "config.json")
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v", fi.Mode().Perm())
		}
	}
	m := readJSON(t, path)
	if m["apiKey"] != "tm_secret" || m["ingestUrl"] != "https://example.test/functions/v1/ingest?a=1&b=2" {
		t.Fatalf("config = %v", m)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestSeedKeyOmitsEmptyIngestURL(t *testing.T) {
	agent := t.TempDir()
	if seeded, err := SeedKey(agent, "tm_k", ""); err != nil || !seeded {
		t.Fatalf("seeded=%v err=%v", seeded, err)
	}
	m := readJSON(t, filepath.Join(agent, "tokenmunchers", "config.json"))
	if _, ok := m["ingestUrl"]; ok {
		t.Fatalf("ingestUrl written: %v", m)
	}
}

func writeConfig(t *testing.T, agent, body string) string {
	t.Helper()
	dir := filepath.Join(agent, "tokenmunchers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSeedKeyPreservesFields(t *testing.T) {
	agent := t.TempDir()
	p := writeConfig(t, agent, `{"extra": {"n": [1, 2]}, "apiKey": "", "ingestUrl": "https://old.test/ingest"}`)
	seeded, err := SeedKey(agent, "tm_new", "")
	if err != nil || !seeded {
		t.Fatalf("seeded=%v err=%v", seeded, err)
	}
	m := readJSON(t, p)
	if m["apiKey"] != "tm_new" || m["ingestUrl"] != "https://old.test/ingest" {
		t.Fatalf("config = %v", m)
	}
	extra, _ := m["extra"].(map[string]any)
	if extra == nil || len(extra["n"].([]any)) != 2 {
		t.Fatalf("unknown field lost: %v", m)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v", fi.Mode().Perm())
		}
	}
	// ingestUrl overridden when provided
	if _, err := SeedKey(agent, "tm_other", "https://x.test/ingest"); err != nil {
		t.Fatal(err)
	}
	if m := readJSON(t, p); m["apiKey"] != "tm_new" {
		t.Fatalf("second seed overwrote key: %v", m)
	}
}

func TestSeedKeyReplacesNonStringKey(t *testing.T) {
	agent := t.TempDir()
	p := writeConfig(t, agent, `{"apiKey": 5}`)
	if seeded, err := SeedKey(agent, "tm_k", "https://x.test/ingest"); err != nil || !seeded {
		t.Fatalf("seeded=%v err=%v", seeded, err)
	}
	m := readJSON(t, p)
	if m["apiKey"] != "tm_k" || m["ingestUrl"] != "https://x.test/ingest" {
		t.Fatalf("config = %v", m)
	}
}

func TestSeedKeyKeepsExistingKey(t *testing.T) {
	agent := t.TempDir()
	body := `{"apiKey":"tm_existing","ingestUrl":"https://old.test/ingest"}`
	p := writeConfig(t, agent, body)
	seeded, err := SeedKey(agent, "tm_new", "https://new.test/ingest")
	if err != nil || seeded {
		t.Fatalf("seeded=%v err=%v", seeded, err)
	}
	if b, _ := os.ReadFile(p); string(b) != body {
		t.Fatalf("file modified: %s", b)
	}
}

func TestSeedKeyRejectsCorrupt(t *testing.T) {
	for name, body := range map[string]string{
		"garbage": `{"apiKey": "tm_x"`,
		"array":   `[1,2]`,
		"null":    `null`,
	} {
		t.Run(name, func(t *testing.T) {
			agent := t.TempDir()
			p := writeConfig(t, agent, body)
			seeded, err := SeedKey(agent, "tm_new", "")
			if err == nil || seeded {
				t.Fatalf("seeded=%v err=%v", seeded, err)
			}
			if strings.Contains(err.Error(), "tm_") {
				t.Fatalf("error leaks key material: %v", err)
			}
			if b, _ := os.ReadFile(p); string(b) != body {
				t.Fatalf("file modified: %s", b)
			}
		})
	}
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestAgentDir(t *testing.T) {
	home := filepath.FromSlash("/home/u")
	abs := filepath.FromSlash("/custom/agent")
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"default", nil, filepath.Join(home, ".omp", "agent")},
		{"PI_CODING_AGENT_DIR", map[string]string{"PI_CODING_AGENT_DIR": abs}, abs},
		{"PI_CONFIG_DIR", map[string]string{"PI_CONFIG_DIR": ".omp-dev"}, filepath.Join(home, ".omp-dev", "agent")},
		{"profile", map[string]string{"OMP_PROFILE": "work"}, filepath.Join(home, ".omp", "profiles", "work", "agent")},
		{"legacy profile", map[string]string{"PI_PROFILE": "work"}, filepath.Join(home, ".omp", "profiles", "work", "agent")},
		{"OMP_PROFILE wins", map[string]string{"OMP_PROFILE": "a", "PI_PROFILE": "b"}, filepath.Join(home, ".omp", "profiles", "a", "agent")},
		{"profile beats dir override", map[string]string{"OMP_PROFILE": "work", "PI_CODING_AGENT_DIR": abs}, filepath.Join(home, ".omp", "profiles", "work", "agent")},
		{"default profile name", map[string]string{"OMP_PROFILE": "default", "PI_CODING_AGENT_DIR": abs}, abs},
		{"invalid profile ignored", map[string]string{"OMP_PROFILE": "Bad/Name"}, filepath.Join(home, ".omp", "agent")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentDir(envMap(tc.env), home); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

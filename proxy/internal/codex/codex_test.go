package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHomeFollowsCodexHome(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	if got, want := Home(env(nil), "/home/ada"), filepath.Join("/home/ada", ".codex"); got != want {
		t.Errorf("Home without CODEX_HOME = %q, want %q", got, want)
	}
	if got, want := Home(env(map[string]string{"CODEX_HOME": ""}), "/home/ada"), filepath.Join("/home/ada", ".codex"); got != want {
		t.Errorf("Home with an empty CODEX_HOME = %q, want %q (Codex ignores an empty value)", got, want)
	}
	if got, want := Home(env(map[string]string{"CODEX_HOME": "/srv/codex"}), "/home/ada"), filepath.Clean("/srv/codex"); got != want {
		t.Errorf("Home with CODEX_HOME = %q, want %q", got, want)
	}
	rel := Home(env(map[string]string{"CODEX_HOME": filepath.Join("relative", "dir")}), "/home/ada")
	if !filepath.IsAbs(rel) || !strings.HasSuffix(rel, filepath.Join("relative", "dir")) {
		t.Errorf("Home with a relative CODEX_HOME = %q, want an absolute path", rel)
	}
	if got, want := ConfigPath("/srv/codex"), filepath.Join("/srv/codex", "config.toml"); got != want {
		t.Errorf("ConfigPath = %q, want %q", got, want)
	}
}

func TestBaseURLIsAMunchRoute(t *testing.T) {
	prefix := "/_tm/" + testCap
	for _, listen := range []string{"127.0.0.1:8787", "127.0.0.2:1", "[::1]:9000"} {
		got := BaseURL(listen, prefix)
		if want := "http://" + listen + prefix + "/chatgpt/backend-api/codex"; got != want {
			t.Errorf("BaseURL(%q) = %q, want %q", listen, got, want)
		}
		if !providerURLRe.MatchString(got) {
			t.Errorf("BaseURL(%q) = %q does not pass munch's own route check", listen, got)
		}
	}
}

func TestIsCanonicalUpstream(t *testing.T) {
	for upstream, want := range map[string]bool{
		"https://chatgpt.com":              true,
		"https://chatgpt.com/":             true,
		"http://chatgpt.com":               false,
		"https://chatgpt.example.test":     false,
		"https://chatgpt.com.example.test": false,
		"https://chatgpt.com/other":        false,
		"":                                 false,
	} {
		if got := IsCanonicalUpstream(upstream); got != want {
			t.Errorf("IsCanonicalUpstream(%q) = %v, want %v", upstream, got, want)
		}
	}
}

// fakeCodex writes a stand-in codex whose `login status` runs body and returns
// its path. Any other command exits 2.
func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in codex is a shell script")
	}
	path := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\n[ \"$1 $2\" = \"login status\" ] || exit 2\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSignedInWithChatGPT(t *testing.T) {
	chatty := `i=0; while [ $i -lt 2000 ]; do echo "padding padding padding padding padding"; i=$((i+1)); done; echo "Logged in using ChatGPT"`
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"ChatGPT on stderr", `echo "Logged in using ChatGPT" >&2`, true},
		{"ChatGPT on stdout", `echo "Logged in using ChatGPT"`, true},
		{"ChatGPT with trailing detail", `echo "Logged in using ChatGPT (Plus)" >&2`, true},
		{"API key", `echo "Logged in using an API key - sk-proj-***ABCDE" >&2`, false},
		{"not logged in", `echo "Not logged in" >&2; exit 1`, false},
		{"mentions ChatGPT but fails", `echo "Logged in using ChatGPT" >&2; exit 1`, false},
		{"silent", `true`, false},
		// Only the start of the output is kept, so a flood cannot hang or bloat the check.
		{"phrase past the output cap", chatty, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, err := SignedInWithChatGPT(ctx, fakeCodex(t, tc.body))
			if err != nil {
				t.Fatalf("SignedInWithChatGPT: %v", err)
			}
			if got != tc.want {
				t.Errorf("SignedInWithChatGPT = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSignedInWithChatGPTIsBounded(t *testing.T) {
	bin := fakeCodex(t, `while :; do :; done`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	got, err := SignedInWithChatGPT(ctx, bin)
	if got || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SignedInWithChatGPT = %v, %v; want false and a deadline error", got, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the check took %v, want it cut off near the deadline", elapsed)
	}
}

func TestSignedInWithChatGPTReportsAMissingBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := SignedInWithChatGPT(ctx, filepath.Join(t.TempDir(), "no-such-codex"))
	if got || err == nil {
		t.Errorf("SignedInWithChatGPT = %v, %v; want false and an error", got, err)
	}
}

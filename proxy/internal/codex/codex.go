// Package codex routes the OpenAI Codex CLI, when it is signed in with
// ChatGPT, through the munch proxy.
//
// Codex ignores the proxy environment variables in that mode and reads its
// model provider from a user-level config.toml instead, so munch manages a
// marked provider block there (see config.go). Nothing in this package opens
// Codex's credential files or prints what Codex says about its sign-in.
package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// statusOutputLimit caps how much `codex login status` output is kept.
	statusOutputLimit = 4 << 10
	// statusWaitDelay bounds how long Wait lingers once the command is killed
	// or has exited while a child still holds its output pipes.
	statusWaitDelay = 2 * time.Second

	canonicalUpstream = "https://chatgpt.com"
)

// Find locates the codex executable on PATH.
func Find() (path string, ok bool) {
	p, err := exec.LookPath("codex")
	if err != nil {
		return "", false
	}
	return p, true
}

// Home returns Codex's user-level directory the way Codex resolves it:
// $CODEX_HOME when it is set and non-empty, otherwise <home>/.codex.
func Home(env func(string) string, home string) string {
	if v := env("CODEX_HOME"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return filepath.Clean(v)
	}
	return filepath.Join(home, ".codex")
}

// ConfigPath returns the user-level config file inside a Codex home directory.
func ConfigPath(dir string) string { return filepath.Join(dir, "config.toml") }

// BaseURL returns the provider base_url that sends Codex through the proxy
// listening on listen (host:port) under the private route prefix
// ("/_tm/<capability>"). The result is a secret: it carries the capability.
func BaseURL(listen, routePrefix string) string {
	return "http://" + listen + routePrefix + "/chatgpt/backend-api/codex"
}

// IsCanonicalUpstream reports whether a chatgpt route upstream is the real
// ChatGPT origin. Codex sends its ChatGPT sign-in credentials with every
// request, so they must never be routed to any other upstream.
func IsCanonicalUpstream(upstream string) bool {
	return strings.TrimRight(upstream, "/") == canonicalUpstream
}

// SignedInWithChatGPT runs `codex login status` and reports whether Codex says
// it is signed in with ChatGPT. The output is read only for that phrase, is
// capped, and is never returned: an API-key sign-in prints part of the key.
// A command that exits non-zero means "not signed in"; one that cannot run or
// does not finish before ctx ends is an error.
func SignedInWithChatGPT(ctx context.Context, bin string) (bool, error) {
	cmd := exec.CommandContext(ctx, bin, "login", "status")
	out := &cappedBuffer{limit: statusOutputLimit}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = statusWaitDelay
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("codex login status: %w", ctxErr)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return strings.Contains(strings.ToLower(out.String()), "logged in using chatgpt"), nil
	case errors.As(err, &exitErr):
		return false, nil
	default:
		return false, fmt.Errorf("codex login status: %w", err)
	}
}

// cappedBuffer keeps the first limit bytes written to it and discards the rest.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

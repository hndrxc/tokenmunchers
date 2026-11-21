// Package omp integrates munch with Oh My Pi (omp) and its
// omp-tokenmunchers plugin, which reports OMP usage natively.
package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// PluginName is the npm name of the OMP plugin that reports OMP usage.
const PluginName = "omp-tokenmunchers"

const maxStderrInError = 512

// Find locates the omp executable on PATH.
func Find() (path string, ok bool) {
	p, err := exec.LookPath("omp")
	if err != nil {
		return "", false
	}
	return p, true
}

// PluginInstalled reports whether omp-tokenmunchers appears in
// `omp plugin list --json` (the "npm" array of {name, version, ...} entries).
func PluginInstalled(ctx context.Context, ompBin string) (bool, error) {
	cmd := exec.CommandContext(ctx, ompBin, "plugin", "list", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("omp plugin list: %w", ctxErr)
		}
		return false, fmt.Errorf("omp plugin list: %w%s", err, stderrSuffix(stderr.Bytes()))
	}
	var out struct {
		Npm []struct {
			Name string `json:"name"`
		} `json:"npm"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return false, fmt.Errorf("omp plugin list: parse output: %w", err)
	}
	for _, p := range out.Npm {
		if p.Name == PluginName {
			return true, nil
		}
	}
	return false, nil
}

func stderrSuffix(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > maxStderrInError {
		s = s[:maxStderrInError] + "..."
	}
	return ": " + s
}

// InstallPlugin runs `<ompBin> plugin install omp-tokenmunchers`, streaming its
// output to stdout/stderr. Stdin is not inherited.
func InstallPlugin(ctx context.Context, ompBin string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, ompBin, "plugin", "install", PluginName)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("omp plugin install %s: %w", PluginName, ctxErr)
		}
		return fmt.Errorf("omp plugin install %s: %w", PluginName, err)
	}
	return nil
}

var profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// normalizeProfile mirrors pi-utils normalizeProfileName; invalid names are
// treated as the default profile (OMP does the same at import time).
func normalizeProfile(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "default" || p == "." || p == ".." || strings.HasSuffix(p, ".") || !profileNameRE.MatchString(p) {
		return ""
	}
	return p
}

// AgentDir resolves OMP's agent directory like pi-utils/src/dirs.ts does:
// the config root is home/${PI_CONFIG_DIR:-.omp}; a named profile
// (OMP_PROFILE, else legacy PI_PROFILE) selects <root>/profiles/<name>/agent;
// otherwise PI_CODING_AGENT_DIR overrides, else <root>/agent.
func AgentDir(env func(string) string, home string) string {
	cfg := env("PI_CONFIG_DIR")
	if cfg == "" {
		cfg = ".omp"
	}
	root := filepath.Join(home, cfg)
	profileSrc := env("PI_PROFILE")
	if v := env("OMP_PROFILE"); v != "" {
		profileSrc = v
	}
	// An explicitly empty OMP_PROFILE selects the default profile and shadows
	// PI_PROFILE; os.Getenv cannot distinguish unset from empty, so empty
	// falls through to PI_PROFILE here.
	if profile := normalizeProfile(profileSrc); profile != "" {
		return filepath.Join(root, "profiles", profile, "agent")
	}
	if override := env("PI_CODING_AGENT_DIR"); override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			return abs
		}
		return filepath.Clean(override)
	}
	return filepath.Join(root, "agent")
}

// SeedKey writes apiKey (and ingestUrl when non-empty) into
// <agentDir>/tokenmunchers/config.json unless the file already holds a
// non-empty apiKey. Other fields are preserved; a corrupt file is an error and
// is left untouched. The write is atomic; dir is 0700 and file 0600.
func SeedKey(agentDir, apiKey, ingestURL string) (seeded bool, err error) {
	dir := filepath.Join(agentDir, "tokenmunchers")
	path := filepath.Join(dir, "config.json")

	fields := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
				return false, fmt.Errorf("%s: not a JSON object; leaving it untouched", path)
			}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return false, err
	}

	if raw, ok := fields["apiKey"]; ok {
		var existing string
		if json.Unmarshal(raw, &existing) == nil && existing != "" {
			return false, nil
		}
	}

	key, err := json.Marshal(apiKey)
	if err != nil {
		return false, err
	}
	fields["apiKey"] = key
	if ingestURL != "" {
		u, err := json.Marshal(ingestURL)
		if err != nil {
			return false, err
		}
		fields["ingestUrl"] = u
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fields); err != nil {
		return false, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return false, err
	}
	return true, nil
}

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Version is the munch release version, defaulting to the development version
// 0.1.0. Release builds override it at link time via
//   -X github.com/tokenmunchers/tokenmunchers/proxy/internal/config.Version=<version>
// where the linked version is the full (e.g. "1.4.1") with no "munch-v"
// prefix.
var Version = "0.1.0"

// ClientVersion is the ingest client identity reported to the upstream
// service; it always carries the "tmproxy/" prefix.
var ClientVersion = "tmproxy/" + Version

const (
	DefaultListen    = "127.0.0.1:8787"
	DefaultIngestURL = "https://fywgkzqgtitpwijweojk.supabase.co/functions/v1/ingest"
)

// KeyPattern matches TokenMunchers API keys (tm_ followed by 43 base64url chars).
var KeyPattern = regexp.MustCompile(`^tm_[A-Za-z0-9_-]{43}$`)

// Format identifies the upstream wire format used for token extraction.
type Format string

const (
	FormatAuto      Format = "auto"
	FormatAnthropic Format = "anthropic"
	FormatOpenAI    Format = "openai"
	FormatGemini    Format = "gemini"
	FormatOllama    Format = "ollama"
)

// Route maps a local URL prefix (e.g. "anthropic") to an upstream base URL.
type Route struct {
	Name     string `json:"-"`
	Upstream string `json:"upstream"`
	Provider string `json:"provider,omitempty"`
	Format   Format `json:"format,omitempty"`
	Local    bool   `json:"local,omitempty"` // true for local providers like Ollama (cost = 0)
}

// Config is the persisted user configuration for munch.
type Config struct {
	ListenAddr string           `json:"listen_addr,omitempty"`
	IngestURL  string           `json:"ingest_url,omitempty"`
	APIKey     string           `json:"api_key,omitempty"`
	Paused     bool             `json:"paused,omitempty"`
	Routes     map[string]Route `json:"routes,omitempty"`

	// OMPViaPlugin records that Oh My Pi reports its usage natively through the
	// omp-tokenmunchers plugin. `munch env` then keeps OMP out of the proxy
	// environment so each OMP call is not reported a second time by the proxy.
	OMPViaPlugin bool `json:"omp_via_plugin,omitempty"`
}

// Paths holds the resolved config and state directories.
type Paths struct {
	ConfigDir string
	StateDir  string
}

// DefaultPaths returns OS-appropriate config and state directories.
// On Linux it follows the XDG Base Directory specification; on macOS/Windows it
// uses standard user directories so the CLI ports cleanly across platforms.
func DefaultPaths() (Paths, error) {
	configDir := strings.TrimSpace(os.Getenv("TMPROXY_CONFIG_DIR"))
	stateDir := strings.TrimSpace(os.Getenv("TMPROXY_STATE_DIR"))

	if configDir == "" {
		if runtime.GOOS == "linux" {
			if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
				configDir = filepath.Join(xdg, "tmproxy")
			} else if home, err := os.UserHomeDir(); err == nil && home != "" {
				configDir = filepath.Join(home, ".config", "tmproxy")
			}
		}
		if configDir == "" {
			base, err := os.UserConfigDir()
			if err != nil {
				return Paths{}, fmt.Errorf("resolve config dir: %w", err)
			}
			configDir = filepath.Join(base, "tmproxy")
		}
	}

	if stateDir == "" {
		if runtime.GOOS == "linux" {
			if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
				stateDir = filepath.Join(xdg, "tmproxy")
			} else if home, err := os.UserHomeDir(); err == nil && home != "" {
				stateDir = filepath.Join(home, ".local", "state", "tmproxy")
			}
		}
		if stateDir == "" {
			base, err := os.UserCacheDir()
			if err != nil {
				base = configDir
			}
			stateDir = filepath.Join(base, "tmproxy")
		}
	}

	return Paths{
		ConfigDir: configDir,
		StateDir:  stateDir,
	}, nil
}

// ConfigFile returns the path to config.json.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.json") }

// PricesFile returns the path to optional user price overrides.
func (p Paths) PricesFile() string { return filepath.Join(p.ConfigDir, "prices.json") }

// QueueFile returns the path to the durable delivery retry queue.
func (p Paths) QueueFile() string { return filepath.Join(p.StateDir, "queue.jsonl") }

// UsageLogFile returns the path to the local metadata-only usage log.
func (p Paths) UsageLogFile() string { return filepath.Join(p.StateDir, "usage.jsonl") }

// PIDFile returns the path to the background daemon PID file.
func (p Paths) PIDFile() string { return filepath.Join(p.StateDir, "tmproxy.pid") }

// LogFile returns the path to the background daemon log file.
func (p Paths) LogFile() string { return filepath.Join(p.StateDir, "tmproxy.log") }

// BuiltinRoutes returns the default inference provider routes.
func BuiltinRoutes() map[string]Route {
	return map[string]Route{
		"anthropic": {
			Name:     "anthropic",
			Upstream: "https://api.anthropic.com",
			Provider: "anthropic",
			Format:   FormatAnthropic,
		},
		"openai": {
			Name:     "openai",
			Upstream: "https://api.openai.com",
			Provider: "openai",
			Format:   FormatOpenAI,
		},
		"chatgpt": {
			Name:     "chatgpt",
			Upstream: "https://chatgpt.com",
			Provider: "openai",
			Format:   FormatOpenAI,
		},
		"gemini": {
			Name:     "gemini",
			Upstream: "https://generativelanguage.googleapis.com",
			Provider: "google",
			Format:   FormatGemini,
		},
		"xai": {
			Name:     "xai",
			Upstream: "https://api.x.ai",
			Provider: "xai",
			Format:   FormatOpenAI,
		},
		"grok": {
			Name:     "grok",
			Upstream: "https://api.x.ai",
			Provider: "xai",
			Format:   FormatOpenAI,
		},
		"openrouter": {
			Name:     "openrouter",
			Upstream: "https://openrouter.ai",
			Provider: "openrouter",
			Format:   FormatOpenAI,
		},
		"ollama": {
			Name:     "ollama",
			Upstream: "http://127.0.0.1:11434",
			Provider: "ollama",
			Format:   FormatOllama,
			Local:    true,
		},
	}
}

// Load reads config.json (if present) and applies defaults and environment overrides.
func Load(paths Paths) (Config, error) {
	cfg, err := LoadFile(paths)
	if err != nil {
		return Config{}, err
	}

	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListen
	}
	if cfg.IngestURL == "" {
		cfg.IngestURL = DefaultIngestURL
	}

	if v := strings.TrimSpace(os.Getenv("TMPROXY_LISTEN")); v != "" {
		cfg.ListenAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("TOKENMUNCHERS_URL")); v != "" {
		cfg.IngestURL = v
	}
	if v := strings.TrimSpace(os.Getenv("TOKENMUNCHERS_KEY")); v != "" {
		cfg.APIKey = v
	} else if v := strings.TrimSpace(os.Getenv("TOKENMUNCHERS_API_KEY")); v != "" {
		cfg.APIKey = v
	}

	merged := BuiltinRoutes()
	seen := make(map[string]bool, len(cfg.Routes))
	for name, r := range cfg.Routes {
		key := strings.ToLower(strings.Trim(strings.TrimSpace(name), "/"))
		if key == "" {
			return Config{}, errors.New("config has a route with an empty name")
		}
		if seen[key] {
			return Config{}, fmt.Errorf("config has colliding route names normalizing to %q", key)
		}
		seen[key] = true
		if err := ValidateRoute(key, r); err != nil {
			return Config{}, err
		}
		if r.Provider == "" {
			r.Provider = key
		}
		if r.Format == "" {
			r.Format = FormatAuto
		}
		r.Name = key
		merged[key] = r
	}
	cfg.Routes = merged

	return cfg, nil
}

// LoadFile reads only what is stored on disk, ignoring env overrides and builtin route merge.
func LoadFile(paths Paths) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", paths.ConfigFile(), err)
	}
	return cfg, nil
}

// Save writes cfg to disk with restrictive permissions (0700 dir, 0600 file).
func Save(paths Paths, cfg Config) error {
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	tmp := paths.ConfigFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod config: %w", err)
	}
	if err := os.Rename(tmp, paths.ConfigFile()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit config: %w", err)
	}
	return nil
}

// ValidateAPIKey checks that key matches TokenMunchers' tm_ key format.
func ValidateAPIKey(key string) error {
	if !KeyPattern.MatchString(key) {
		return fmt.Errorf("invalid API key format (expected tm_ followed by 43 characters)")
	}
	return nil
}

// MaskAPIKey returns a safe display prefix for status output.
func MaskAPIKey(key string) string {
	if key == "" {
		return "(not configured)"
	}
	if len(key) <= 10 {
		return key[:min(4, len(key))] + "..."
	}
	return key[:10] + "..."
}

// ValidateRoute checks that a route has a safe upstream: absolute https with a
// hostname, or cleartext http only to a numeric loopback address. Userinfo,
// query and fragment are rejected. Errors never include the raw URL.
func ValidateRoute(name string, r Route) error {
	if name == "" || strings.HasPrefix(name, "_") {
		return fmt.Errorf("invalid route name %q", name)
	}
	bad := fmt.Errorf("route %q has an invalid upstream URL", name)
	u, err := url.Parse(r.Upstream)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || strings.Contains(r.Upstream, "#") {
		return bad
	}
	host := u.Hostname()
	if host == "" {
		return bad
	}
	switch u.Scheme {
	case "https":
	case "http":
		addr, err := netip.ParseAddr(host)
		if err != nil || addr.Zone() != "" || !addr.Unmap().IsLoopback() {
			return fmt.Errorf("route %q has an invalid upstream URL (cleartext http requires a loopback address)", name)
		}
	default:
		return bad
	}
	return nil
}

// NormalizeListenAddr returns a canonical numeric-loopback host:port. Blank
// input yields DefaultListen; "localhost" maps to 127.0.0.1 without DNS.
func NormalizeListenAddr(raw string) (string, error) {
	bad := errors.New("invalid listen address: must be a loopback host:port")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultListen
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || port == "" {
		return "", bad
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", bad
		}
	}
	if len(port) > 5 {
		return "", bad
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", bad
	}
	var addr netip.Addr
	if strings.EqualFold(host, "localhost") {
		addr = netip.MustParseAddr("127.0.0.1")
	} else {
		addr, err = netip.ParseAddr(host)
		if err != nil || addr.Zone() != "" {
			return "", bad
		}
		addr = addr.Unmap()
		if !addr.IsLoopback() {
			return "", bad
		}
	}
	return net.JoinHostPort(addr.String(), strconv.Itoa(n)), nil
}

// SortedRouteNames returns route names in deterministic alphabetical order.
func (c Config) SortedRouteNames() []string {
	names := make([]string, 0, len(c.Routes))
	for k := range c.Routes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

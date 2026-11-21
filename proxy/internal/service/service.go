package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
)

// Info describes the current background service installation and state.
type Info struct {
	Backend   string `json:"backend"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	UnitPath  string `json:"unit_path,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Manager abstracts background daemon lifecycle so Linux (systemd user unit),
// macOS (launchd), and fallback PID-file daemons share one CLI interface.
type Manager interface {
	Backend() string
	Install(binaryPath string) (string, error)
	Uninstall() error
	Start(binaryPath string) error
	Stop() error
	Status() Info
}

// NewManager returns the OS-appropriate service manager. The manager loads the
// local control key from paths.ConfigDir itself whenever it needs to talk to
// the daemon; it never creates the key (the daemon does that on startup).
func NewManager(paths config.Paths, listenAddr string) Manager {
	return newPlatformManager(paths, listenAddr)
}

const (
	// maxHealthBodyBytes bounds the /_tmproxy/health reply, which is a tiny JSON object.
	maxHealthBodyBytes = 4 << 10
	// maxControlBodyBytes bounds other control replies (status, flush, ...).
	maxControlBodyBytes = 1 << 20

	challengeHeader = "X-TMProxy-Challenge"
	controlHeader   = "X-TMProxy-Control"
)

// errConnChanged is returned when the connection that carried the daemon's
// authentication proof is no longer the one a control request would use.
var errConnChanged = errors.New("connection to munch changed after authentication")

// validAction restricts control actions to a safe path segment.
var validAction = regexp.MustCompile(`^[a-z0-9_-]+$`)

// newControlClient returns an HTTP client for local control calls. It never
// follows redirects so a rogue listener cannot bounce control requests elsewhere,
// and it never uses an HTTP proxy. With singleConn the client may dial exactly
// once, so a follow-up request can only travel over the connection that was
// already authenticated.
func newControlClient(timeout time.Duration, singleConn bool) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	var dials atomic.Int32
	tr := &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: 1,
		DisableCompression:  true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if singleConn && dials.Add(1) > 1 {
				return nil, errConnChanged
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// readLimited reads at most max bytes from r and errors if the body is larger.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("control response exceeds %d bytes", max)
	}
	return body, nil
}

// verifyHealthBody validates the health identity and that its proof answers
// nonce under key (constant-time; an invalid or zero key never verifies).
func verifyHealthBody(body []byte, nonce string, key localauth.Key) bool {
	var h struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		PID     int    `json:"pid"`
		Proof   string `json:"proof"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return false
	}
	if !h.OK || h.Version == "" || h.PID <= 0 {
		return false
	}
	return key.VerifyProof(nonce, h.Proof)
}

// challengeHealth sends a fresh random challenge to /_tmproxy/health over
// client and returns the health body only if the peer proves it holds key.
// No credential is sent: the challenge is public randomness.
func challengeHealth(ctx context.Context, client *http.Client, listenAddr string, key localauth.Key) ([]byte, error) {
	nonce, err := localauth.NewNonce()
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://%s/_tmproxy/health", normalizeClientAddr(listenAddr))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(challengeHeader, nonce)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, maxHealthBodyBytes)
	if err != nil {
		return nil, err
	}
	if !verifyHealthBody(body, nonce, key) {
		return nil, errors.New("listener did not prove it is this user's munch")
	}
	return body, nil
}

// ProbeHealth reports whether the munch daemon holding key is responding on
// listenAddr. It sends a random challenge and requires a bounded health reply
// (ok, version, positive pid) carrying a valid proof for that challenge, so an
// arbitrary listener on the port is never accepted as the daemon.
func ProbeHealth(ctx context.Context, listenAddr string, key localauth.Key) bool {
	client := newControlClient(800*time.Millisecond, false)
	defer client.CloseIdleConnections()
	_, err := challengeHealth(ctx, client, listenAddr, key)
	return err == nil
}

// probeDaemon is ProbeHealth with the key loaded from paths.ConfigDir. A
// missing or unreadable key means no daemon for this config can be proven.
func probeDaemon(ctx context.Context, paths config.Paths, listenAddr string) bool {
	key, err := localauth.Load(paths.ConfigDir)
	if err != nil {
		return false
	}
	return ProbeHealth(ctx, listenAddr, key)
}

// doControl verifies the daemon's proof first and only then sends the control
// token, over the very connection that was verified. The token is never sent
// to a peer that failed authentication.
func doControl(ctx context.Context, method string, timeout time.Duration, listenAddr, action string, key localauth.Key) ([]byte, int, error) {
	if !validAction.MatchString(action) {
		return nil, 0, fmt.Errorf("invalid control action %q", action)
	}
	token := key.ControlToken()
	if token == "" {
		return nil, 0, errors.New("local control key is invalid")
	}
	client := newControlClient(timeout, true)
	defer client.CloseIdleConnections()

	health, err := challengeHealth(ctx, client, listenAddr, key)
	if err != nil {
		return nil, 0, fmt.Errorf("munch authentication failed: %w", err)
	}
	if method == http.MethodGet && action == "health" {
		return health, http.StatusOK, nil
	}

	url := fmt.Sprintf("http://%s/_tmproxy/%s", normalizeClientAddr(listenAddr), action)
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set(controlHeader, token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxControlBodyBytes)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// PostControl sends an authenticated POST to a local /_tmproxy/<action> endpoint.
func PostControl(ctx context.Context, listenAddr, action string, key localauth.Key) ([]byte, int, error) {
	return doControl(ctx, http.MethodPost, 10*time.Second, listenAddr, action, key)
}

// GetControl sends an authenticated GET to a local /_tmproxy/<action> endpoint.
// For "health" it returns the proof-verified health body.
func GetControl(ctx context.Context, listenAddr, action string, key localauth.Key) ([]byte, int, error) {
	return doControl(ctx, http.MethodGet, 3*time.Second, listenAddr, action, key)
}

func normalizeClientAddr(listenAddr string) string {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return config.DefaultListen
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return addr
}

// listenerPresent reports whether anything accepts TCP connections on listenAddr.
func listenerPresent(listenAddr string) bool {
	conn, err := net.DialTimeout("tcp", normalizeClientAddr(listenAddr), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// pidManager provides a portable background daemon fallback using a detached child process,
// an informational PID file, and the authenticated /_tmproxy/shutdown control API.
// It never signals a process by PID: only a daemon that proves possession of the
// local key can be stopped.
type pidManager struct {
	paths      config.Paths
	listenAddr string
}

func (m *pidManager) Backend() string { return "pidfile" }

func (m *pidManager) Install(_ string) (string, error) {
	return "", errors.New("auto-start service installation is not supported by the fallback pidfile backend on this OS")
}

func (m *pidManager) Uninstall() error {
	return nil
}

func (m *pidManager) Start(binaryPath string) error {
	if probeDaemon(context.Background(), m.paths, m.listenAddr) {
		return nil
	}
	if err := os.MkdirAll(m.paths.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	logFile, err := os.OpenFile(m.paths.LogFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	args := []string{
		"run",
		"--config-dir", m.paths.ConfigDir,
		"--state-dir", m.paths.StateDir,
		"--listen", m.listenAddr,
	}
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start background process: %w", err)
	}
	_ = os.WriteFile(m.paths.PIDFile(), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	_ = cmd.Process.Release()

	// Wait briefly for the listener to become ready. The daemon creates the
	// key on first run, so it is reloaded on every attempt.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if probeDaemon(context.Background(), m.paths, m.listenAddr) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("munch started (pid %d) but did not respond on %s; check %s", cmd.Process.Pid, m.listenAddr, m.paths.LogFile())
}

// Stop asks an authenticated daemon to shut down. If no daemon can be proven
// and something else is listening, Stop fails rather than guess; if nothing is
// listening there is nothing to stop and a stale PID file is removed.
func (m *pidManager) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, err := localauth.Load(m.paths.ConfigDir)
	if err == nil && ProbeHealth(ctx, m.listenAddr, key) {
		_, code, err := PostControl(ctx, m.listenAddr, "shutdown", key)
		if err != nil {
			return fmt.Errorf("stop munch: %w", err)
		}
		if code < 200 || code > 299 {
			return fmt.Errorf("stop munch: shutdown request returned HTTP %d", code)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if !ProbeHealth(context.Background(), m.listenAddr, key) {
				_ = os.Remove(m.paths.PIDFile())
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("stop munch: daemon on %s did not shut down", m.listenAddr)
	}
	if listenerPresent(m.listenAddr) {
		return fmt.Errorf("stop munch: listener on %s could not be verified as this user's munch; not stopping it", m.listenAddr)
	}
	_ = os.Remove(m.paths.PIDFile())
	return nil
}

func (m *pidManager) Status() Info {
	running := probeDaemon(context.Background(), m.paths, m.listenAddr)
	return Info{
		Backend:   "pidfile",
		Installed: false,
		Running:   running,
		UnitPath:  filepath.Clean(m.paths.PIDFile()),
	}
}

// ParseHealthJSON extracts version and pid from a /_tmproxy/health body.
func ParseHealthJSON(body []byte) (version string, pid int) {
	var h struct {
		Version string `json:"version"`
		PID     int    `json:"pid"`
	}
	_ = json.Unmarshal(body, &h)
	return h.Version, h.PID
}

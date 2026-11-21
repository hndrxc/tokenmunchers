package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
)

var nonceRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func newKey(t *testing.T) (localauth.Key, string) {
	t.Helper()
	// LoadOrCreate requires a private dir; create a fresh subdir rather than
	// using t.TempDir() directly, whose mode depends on the harness.
	dir := filepath.Join(t.TempDir(), "cfg")
	key, err := localauth.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return key, dir
}

func healthReply(nonce string, key localauth.Key) []byte {
	b, _ := json.Marshal(map[string]any{
		"ok": true, "version": "0.1.0", "pid": 1234, "proof": key.Proof(nonce),
	})
	return b
}

// fakeDaemon serves the daemon side of the local protocol: a proof-bearing
// health endpoint and control endpoints gated by the control token.
type fakeDaemon struct {
	addr       string
	mu         sync.Mutex
	nonces     []string
	controlHit atomic.Int32
}

func startDaemon(t *testing.T, key localauth.Key, control http.HandlerFunc) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_tmproxy/health" {
			if r.Header.Get(controlHeader) != "" {
				t.Error("control token sent to health endpoint")
			}
			nonce := r.Header.Get(challengeHeader)
			d.mu.Lock()
			d.nonces = append(d.nonces, nonce)
			d.mu.Unlock()
			_, _ = w.Write(healthReply(nonce, key))
			return
		}
		if r.Header.Get(controlHeader) != key.ControlToken() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		d.controlHit.Add(1)
		control(w, r)
	}))
	t.Cleanup(srv.Close)
	d.addr = strings.TrimPrefix(srv.URL, "http://")
	return d
}

func okControl(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_tmproxy/pause" && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func serveBody(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestAuthenticatedHealthAndControl(t *testing.T) {
	key, _ := newKey(t)
	d := startDaemon(t, key, okControl)

	if !ProbeHealth(context.Background(), d.addr, key) {
		t.Fatalf("expected ProbeHealth=true on %s", d.addr)
	}
	body, code, err := GetControl(context.Background(), d.addr, "health", key)
	if err != nil || code != 200 {
		t.Fatalf("GetControl health failed: %v %d", err, code)
	}
	ver, pid := ParseHealthJSON(body)
	if ver != "0.1.0" || pid != 1234 {
		t.Fatalf("unexpected ParseHealthJSON: ver=%q pid=%d", ver, pid)
	}
	if _, code, err = GetControl(context.Background(), d.addr, "status", key); err != nil || code != 200 {
		t.Fatalf("GetControl status failed: %v %d", err, code)
	}
	if _, code, err = PostControl(context.Background(), d.addr, "pause", key); err != nil || code != 200 {
		t.Fatalf("PostControl pause failed: %v %d", err, code)
	}

	// Every probe uses a fresh, well-formed random challenge.
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := map[string]bool{}
	for _, n := range d.nonces {
		if !nonceRe.MatchString(n) {
			t.Fatalf("malformed challenge %q", n)
		}
		if seen[n] {
			t.Fatalf("challenge %q reused", n)
		}
		seen[n] = true
	}
	if len(d.nonces) < 4 {
		t.Fatalf("expected a challenge per call, saw %d", len(d.nonces))
	}
}

func TestWrongControlTokenIsRejectedByDaemon(t *testing.T) {
	key, _ := newKey(t)
	other, _ := newKey(t)
	d := startDaemon(t, key, okControl)
	// A client holding a different key cannot authenticate the daemon at all.
	if ProbeHealth(context.Background(), d.addr, other) {
		t.Fatal("ProbeHealth accepted daemon proving a different key")
	}
	if _, _, err := PostControl(context.Background(), d.addr, "pause", other); err == nil {
		t.Fatal("PostControl succeeded with the wrong key")
	}
	if n := d.controlHit.Load(); n != 0 {
		t.Fatalf("daemon accepted %d control requests", n)
	}
}

func TestControlTokenNeverSentToUnauthenticatedPeer(t *testing.T) {
	key, _ := newKey(t)
	other, _ := newKey(t)
	cases := map[string]http.HandlerFunc{
		"no proof": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true,"version":"0.1.0","pid":1}`))
		},
		"wrong proof": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(healthReply(r.Header.Get(challengeHeader), other))
		},
		"replayed proof": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(healthReply(strings.Repeat("0", 64), key))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			var leaked atomic.Bool
			addr := serveBody(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(controlHeader) != "" {
					leaked.Store(true)
				}
				h(w, r)
			})
			if ProbeHealth(context.Background(), addr, key) {
				t.Fatal("ProbeHealth accepted unauthenticated peer")
			}
			if _, _, err := GetControl(context.Background(), addr, "status", key); err == nil {
				t.Fatal("GetControl talked to unauthenticated peer")
			}
			if _, _, err := PostControl(context.Background(), addr, "shutdown", key); err == nil {
				t.Fatal("PostControl talked to unauthenticated peer")
			}
			if leaked.Load() {
				t.Fatal("control token was sent to an unauthenticated peer")
			}
		})
	}
}

func TestZeroKeyIsRejected(t *testing.T) {
	key, _ := newKey(t)
	d := startDaemon(t, key, okControl)
	var zero localauth.Key
	if ProbeHealth(context.Background(), d.addr, zero) {
		t.Fatal("ProbeHealth accepted zero key")
	}
	if _, _, err := PostControl(context.Background(), d.addr, "pause", zero); err == nil {
		t.Fatal("PostControl accepted zero key")
	}
}

func TestProbeHealthRejectsMalformedHealth(t *testing.T) {
	key, _ := newKey(t)
	good := func(nonce string, mut map[string]any) []byte {
		m := map[string]any{"ok": true, "version": "0.1.0", "pid": 1, "proof": key.Proof(nonce)}
		for k, v := range mut {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	cases := map[string]func(nonce string) []byte{
		"empty":       func(string) []byte { return nil },
		"html":        func(string) []byte { return []byte(`<html>ok</html>`) },
		"not ok":      func(n string) []byte { return good(n, map[string]any{"ok": false}) },
		"no version":  func(n string) []byte { return good(n, map[string]any{"version": ""}) },
		"zero pid":    func(n string) []byte { return good(n, map[string]any{"pid": 0}) },
		"negative":    func(n string) []byte { return good(n, map[string]any{"pid": -5}) },
		"wrong types": func(n string) []byte { return good(n, map[string]any{"ok": "true", "version": 1, "pid": "1"}) },
		"no proof":    func(n string) []byte { return good(n, map[string]any{"proof": ""}) },
		"oversize": func(n string) []byte {
			return good(n, map[string]any{"pad": strings.Repeat("a", maxHealthBodyBytes)})
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			addr := serveBody(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(mk(r.Header.Get(challengeHeader)))
			})
			if ProbeHealth(context.Background(), addr, key) {
				t.Fatalf("ProbeHealth accepted %q", name)
			}
		})
	}
}

func TestProbeHealthRejectsNon200(t *testing.T) {
	key, _ := newKey(t)
	addr := serveBody(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(healthReply(r.Header.Get(challengeHeader), key))
	})
	if ProbeHealth(context.Background(), addr, key) {
		t.Fatal("ProbeHealth accepted non-200 reply")
	}
}

func TestControlClientsDoNotFollowRedirects(t *testing.T) {
	key, _ := newKey(t)
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write(healthReply(r.Header.Get(challengeHeader), key))
	}))
	defer target.Close()
	addr := serveBody(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/_tmproxy/health", http.StatusTemporaryRedirect)
	})

	if ProbeHealth(context.Background(), addr, key) {
		t.Fatal("ProbeHealth followed redirect")
	}
	if _, _, err := GetControl(context.Background(), addr, "status", key); err == nil {
		t.Fatal("GetControl followed or accepted redirect")
	}
	if _, _, err := PostControl(context.Background(), addr, "pause", key); err == nil {
		t.Fatal("PostControl followed or accepted redirect")
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
}

func TestControlRedirectAfterAuthenticationIsNotFollowed(t *testing.T) {
	key, _ := newKey(t)
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()
	d := startDaemon(t, key, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, code, err := PostControl(context.Background(), d.addr, "pause", key)
	if err != nil || code != http.StatusTemporaryRedirect {
		t.Fatalf("PostControl: code=%d err=%v, want 307 without following", code, err)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
}

func TestControlHelpersCapResponseBody(t *testing.T) {
	key, _ := newKey(t)
	d := startDaemon(t, key, func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		for written := 0; written <= maxControlBodyBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	if _, _, err := GetControl(context.Background(), d.addr, "status", key); err == nil {
		t.Fatal("GetControl accepted oversize body")
	}
	if _, _, err := PostControl(context.Background(), d.addr, "flush", key); err == nil {
		t.Fatal("PostControl accepted oversize body")
	}
}

func TestReadLimitedBoundary(t *testing.T) {
	if b, err := readLimited(strings.NewReader("abcd"), 4); err != nil || string(b) != "abcd" {
		t.Fatalf("exact-size body: %q %v", b, err)
	}
	if _, err := readLimited(strings.NewReader("abcde"), 4); err == nil {
		t.Fatal("expected error for body over limit")
	}
}

func TestPlatformManagerCreation(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	mgr := NewManager(paths, "127.0.0.1:8787")
	if mgr == nil || mgr.Backend() == "" {
		t.Fatalf("expected non-empty service manager backend for %s", runtime.GOOS)
	}
}

func TestManagerDoesNotCreateKey(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "cfg")
	paths := config.Paths{ConfigDir: cfg, StateDir: t.TempDir()}
	m := &pidManager{paths: paths, listenAddr: "127.0.0.1:1"}
	if m.Status().Running {
		t.Fatal("status reported running without a daemon")
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("manager created config dir (stat err=%v)", err)
	}
}

func TestPidManagerStop(t *testing.T) {
	t.Run("authenticated daemon is shut down", func(t *testing.T) {
		key, dir := newKey(t)
		var down atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/_tmproxy/health":
				if down.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write(healthReply(r.Header.Get(challengeHeader), key))
			case r.URL.Path == "/_tmproxy/shutdown" && r.Header.Get(controlHeader) == key.ControlToken():
				down.Store(true)
				_, _ = w.Write([]byte(`{"ok":true}`))
			default:
				w.WriteHeader(http.StatusUnauthorized)
			}
		}))
		defer srv.Close()
		paths := config.Paths{ConfigDir: dir, StateDir: t.TempDir()}
		m := &pidManager{paths: paths, listenAddr: strings.TrimPrefix(srv.URL, "http://")}
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if !down.Load() {
			t.Fatal("shutdown was not requested")
		}
	})

	t.Run("unauthenticated listener is never stopped or trusted", func(t *testing.T) {
		_, dir := newKey(t)
		var leaked, shutdown atomic.Bool
		addr := serveBody(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(controlHeader) != "" {
				leaked.Store(true)
			}
			if strings.HasSuffix(r.URL.Path, "/shutdown") {
				shutdown.Store(true)
			}
			_, _ = w.Write([]byte(`{"ok":true,"version":"0.1.0","pid":1}`))
		})
		paths := config.Paths{ConfigDir: dir, StateDir: t.TempDir()}
		// A stale PID file naming this very test process must never be signalled.
		pidFile := paths.PIDFile()
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := &pidManager{paths: paths, listenAddr: addr}
		if err := m.Stop(); err == nil {
			t.Fatal("Stop succeeded without a proven daemon")
		}
		if leaked.Load() || shutdown.Load() {
			t.Fatal("control request reached an unauthenticated listener")
		}
	})

	t.Run("nothing listening clears stale pid file", func(t *testing.T) {
		_, dir := newKey(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		paths := config.Paths{ConfigDir: dir, StateDir: t.TempDir()}
		if err := os.WriteFile(paths.PIDFile(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := &pidManager{paths: paths, listenAddr: addr}
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop with no daemon: %v", err)
		}
		if _, err := os.Stat(paths.PIDFile()); !os.IsNotExist(err) {
			t.Fatalf("stale pid file not removed (err=%v)", err)
		}
	})
}

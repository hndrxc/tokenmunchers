package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/extract"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/ingest"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localstore"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/pricing"
)

const (
	// MaxRequestModelPeek caps how many bytes of a POST request body we peek
	// (purely in memory, immediately discarded) to extract a fallback "model" field.
	MaxRequestModelPeek = 64 * 1024
	streamChunkSize     = 32 * 1024

	// controlHeader carries the control credential; challengeHeader carries the
	// health-probe nonce. Neither is ever forwarded upstream.
	controlHeader   = "X-TMProxy-Control"
	challengeHeader = "X-TMProxy-Challenge"
)

// StatusSnapshot describes live runtime state returned by GET /_tmproxy/status.
type StatusSnapshot struct {
	Version      string         `json:"version"`
	PID          int            `json:"pid"`
	ListenAddr   string         `json:"listen_addr"`
	IngestURL    string         `json:"ingest_url"`
	APIKeyMasked string         `json:"api_key"`
	HasAPIKey    bool           `json:"has_api_key"`
	Paused       bool           `json:"paused"`
	UptimeSec    int64          `json:"uptime_sec"`
	Captured     int64          `json:"captured"`
	SkippedPause int64          `json:"skipped_paused"`
	Routes       []string       `json:"routes"`
	Ingest       ingest.Stats   `json:"ingest"`
	Desktop      *DesktopStatus `json:"desktop,omitempty"`
}

// ClaudeLogsPath is the local-only OTLP logs endpoint after the capability.
const ClaudeLogsPath = "/__desktop/v1/logs"

// DesktopStatus contains only safe, local accounting diagnostics.
type DesktopStatus struct {
	Pending *int   `json:"pending,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Options wires dependencies into the reverse proxy Server.
type Options struct {
	Config        config.Config
	Paths         config.Paths
	Pricing       *pricing.Table
	Reporter      *ingest.Reporter
	LocalStore    *localstore.Store
	UpstreamHTTP  http.RoundTripper
	Logger        *log.Logger
	Verbose       bool
	OnShutdownReq func()
	// OnReady is invoked by ListenAndServe after a successful bind, before serving.
	OnReady       func()
	ClaudeLogs    http.Handler
	OnPauseChange func(bool) error
	DesktopStatus func() *DesktopStatus
	Now           func() time.Time
	// LocalKey is mandatory; it derives the inference capability prefix and
	// the control credential. The zero key is rejected.
	LocalKey localauth.Key
}

// Server is the local reverse proxy and control API server.
type Server struct {
	cfg           config.Config
	paths         config.Paths
	pricing       *pricing.Table
	reporter      *ingest.Reporter
	localStore    *localstore.Store
	transport     http.RoundTripper
	logger        *log.Logger
	verbose       bool
	onShutdownReq func()
	onReady       func()
	claudeLogs    http.Handler
	onPauseChange func(bool) error
	desktopStatus func() *DesktopStatus
	pauseMu       sync.Mutex
	now           func() time.Time
	sessionKey    [32]byte
	localKey      localauth.Key
	routePrefix   string
	controlToken  string

	startedAt    time.Time
	listenAddr   string
	paused       atomic.Bool
	captured     atomic.Int64
	skippedPause atomic.Int64

	routes  map[string]compiledRoute
	mu      sync.RWMutex
	httpSrv *http.Server
}

type compiledRoute struct {
	cfg    config.Route
	target *url.URL
}

// New creates a new proxy Server.
func New(opts Options) (*Server, error) {
	addr, err := config.NormalizeListenAddr(opts.Config.ListenAddr)
	if err != nil {
		return nil, err
	}
	opts.Config.ListenAddr = addr
	if opts.LocalKey == (localauth.Key{}) {
		return nil, errors.New("local authentication key is required")
	}
	routePrefix := opts.LocalKey.RoutePrefix()
	controlToken := opts.LocalKey.ControlToken()
	if routePrefix == "" || controlToken == "" || strings.HasSuffix(routePrefix, "/") {
		return nil, errors.New("invalid local authentication key")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "munch: ", log.LstdFlags)
	}
	pr := opts.Pricing
	if pr == nil {
		pr = pricing.NewTable(nil)
	}
	tr := opts.UpstreamHTTP
	if tr == nil {
		tr = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// Disable automatic gzip decompression so we can forward bytes verbatim
			// while still decompressing a copy inside extract.Tap when needed.
			DisableCompression: true,
		}
	}

	routes := make(map[string]compiledRoute, len(opts.Config.Routes))
	for name, r := range opts.Config.Routes {
		if err := config.ValidateRoute(name, r); err != nil {
			return nil, err
		}
		u, err := url.Parse(r.Upstream)
		if err != nil {
			return nil, fmt.Errorf("parse route %q upstream: %w", name, err)
		}
		r.Name = name
		routes[name] = compiledRoute{cfg: r, target: u}
	}

	s := &Server{
		cfg:           opts.Config,
		paths:         opts.Paths,
		pricing:       pr,
		reporter:      opts.Reporter,
		localStore:    opts.LocalStore,
		transport:     tr,
		logger:        logger,
		verbose:       opts.Verbose,
		onShutdownReq: opts.OnShutdownReq,
		onReady:       opts.OnReady,
		claudeLogs:    opts.ClaudeLogs,
		onPauseChange: opts.OnPauseChange,
		desktopStatus: opts.DesktopStatus,
		now:           nowFn,
		startedAt:     nowFn(),
		listenAddr:    opts.Config.ListenAddr,
		routes:        routes,
		localKey:      opts.LocalKey,
		routePrefix:   routePrefix,
		controlToken:  controlToken,
	}
	if _, err := rand.Read(s.sessionKey[:]); err != nil {
		return nil, fmt.Errorf("generate session pseudonym key: %w", err)
	}
	s.paused.Store(opts.Config.Paused)
	return s, nil
}

// ListenAndServe starts serving on the configured listen address until ctx is canceled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	defer ln.Close()
	s.mu.Lock()
	s.listenAddr = ln.Addr().String()
	s.httpSrv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	srv := s.httpSrv
	s.mu.Unlock()

	// Bind succeeded; announce readiness before serving begins.
	if s.onReady != nil {
		s.onReady()
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	select {
	case <-ctx.Done():
		shutdownErr := shutdownHTTP(srv)
		return errors.Join(<-errCh, shutdownErr)
	case err := <-errCh:
		return errors.Join(err, shutdownHTTP(srv))
	}
}

func shutdownHTTP(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

// Handler exposes Server as an http.Handler (also convenient for httptest).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())

	// Host protects against DNS rebinding. Fetch metadata also covers browser
	// navigations and other requests that omit Origin.
	if !isLoopbackHost(r.Host) || hasBrowserHeaders(r.Header) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden_host_or_origin"})
		return
	}

	if strings.HasPrefix(r.URL.Path, "/_tmproxy/") {
		s.handleControl(w, r)
		return
	}

	// Inference traffic must carry the secret capability prefix; anything else
	// (including the legacy unprefixed URLs) fails closed before any side effect.
	rest, ok := s.stripCapability(r.URL.EscapedPath())
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}

	// Reject WebSocket upgrades cleanly so clients (e.g. Codex) fall back to HTTP SSE.
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		w.Header().Set("Upgrade", "HTTP/1.1")
		writeJSON(w, http.StatusUpgradeRequired, map[string]string{"error": "websocket_unsupported_use_http"})
		return
	}

	routeName, subPath, ok := splitRoutePath(rest)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":  "unknown_route",
			"routes": s.cfg.SortedRouteNames(),
		})
		return
	}

	// The desktop namespace is reserved even when collection is disabled.
	if routeName == "__desktop" {
		if rest == ClaudeLogsPath && s.claudeLogs != nil {
			s.claudeLogs.ServeHTTP(w, r)
		} else {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		}
		return
	}

	cr, exists := s.routes[routeName]
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":  "unknown_route",
			"route":  routeName,
			"routes": s.cfg.SortedRouteNames(),
		})
		return
	}

	s.proxyRequest(w, r, cr, subPath)
}

// stripCapability matches the literal escaped capability prefix in constant
// time and returns the remaining escaped path. The prefix must be followed by
// '/' or end of path, so no decoding or normalization ambiguity exists.
func (s *Server) stripCapability(escaped string) (string, bool) {
	n := len(s.routePrefix)
	if len(escaped) < n ||
		subtle.ConstantTimeCompare([]byte(escaped[:n]), []byte(s.routePrefix)) != 1 {
		return "", false
	}
	rest := escaped[n:]
	if rest != "" && rest[0] != '/' {
		return "", false
	}
	return rest, true
}

func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, cr compiledRoute, escapedSub string) {
	subPath, err := decodeSubPath(escapedSub)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_path"})
		return
	}
	targetURL := joinUpstreamURL(cr.target, escapedSub, subPath, r.URL.RawQuery)

	var bodyReader io.Reader = r.Body
	var hint extract.RequestHint
	hint.Path = subPath

	isPOST := r.Method == http.MethodPost
	if isPOST && r.Body != nil {
		// Peek up to MaxRequestModelPeek bytes purely to grab a fallback model name,
		// then stitch the exact bytes back ahead of the remaining body stream.
		peekBuf := make([]byte, MaxRequestModelPeek)
		n, err := io.ReadFull(r.Body, peekBuf)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			http.Error(w, "failed reading request body", http.StatusBadRequest)
			return
		}
		peeked := peekBuf[:n]
		hint.Model = extract.PeekRequestModel(subPath, peeked)
		bodyReader = io.MultiReader(bytes.NewReader(peeked), r.Body)
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bodyReader)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad_upstream_request"})
		return
	}
	copyHeaders(upReq.Header, r.Header)
	stripHopByHop(upReq.Header)
	stripLocalAuth(upReq.Header)
	upReq.Host = targetURL.Host
	if r.ContentLength >= 0 {
		upReq.ContentLength = r.ContentLength
	}
	if r.ContentLength == 0 {
		upReq.Body = http.NoBody
	}
	if isPOST {
		// Request uncompressed streams for lowest streaming latency and cleanest SSE parsing.
		upReq.Header.Set("Accept-Encoding", "identity")
	}

	resp, err := s.transport.RoundTrip(upReq)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_unreachable"})
		return
	}
	defer resp.Body.Close()

	// Upstream redirects would hand the client a Location it could replay
	// (with a local Referer); never forward them.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified &&
		resp.Header.Get("Location") != "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_redirect_blocked"})
		return
	}

	// Forward upstream headers and status; do not let upstream documents run
	// active content under the loopback origin or navigate a browser.
	dstHdr := w.Header()
	copyHeaders(dstHdr, resp.Header)
	stripHopByHop(dstHdr)
	dstHdr.Del("Refresh")
	setSecurityHeaders(dstHdr)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	// Only tap successful POST inference responses.
	var tap *extract.Tap
	if isPOST && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		tap = extract.NewTap(
			cr.cfg.Format,
			resp.Header.Get("Content-Type"),
			resp.Header.Get("Content-Encoding"),
			hint,
		)
	}

	buf := make([]byte, streamChunkSize)
	clientDisconnected := false
	upstreamFailed := false
	// A non-EOF upstream body failure must not look like a complete response to
	// the client: abort the handler (after accounting below) so the connection
	// is torn down instead of terminating the stream cleanly.
	defer func() {
		if upstreamFailed {
			panic(http.ErrAbortHandler)
		}
	}()
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if tap != nil {
				_, _ = tap.Write(chunk)
			}
			if _, writeErr := w.Write(chunk); writeErr != nil {
				clientDisconnected = true
				break
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				if r.Context().Err() != nil {
					clientDisconnected = true
				} else {
					upstreamFailed = true
				}
			}
			break
		}
	}

	if tap == nil {
		return
	}

	records := tap.Finalize(clientDisconnected || upstreamFailed)
	if len(records) == 0 {
		return
	}

	if s.paused.Load() {
		s.skippedPause.Add(int64(len(records)))
		return
	}

	sessionID := s.deriveSessionID(r, cr.cfg.Name)
	reqHeaderID := firstNonEmpty(
		resp.Header.Get("request-id"),
		resp.Header.Get("x-request-id"),
		resp.Header.Get("anthropic-request-id"),
	)
	now := s.now().UTC()
	tsStr := now.Format("2006-01-02T15:04:05.000Z")

	for i, rec := range records {
		costUSD, estimated := s.resolveCost(cr.cfg, rec)
		eventID := deriveEventID(cr.cfg.Provider, rec, reqHeaderID, i)

		ev := ingest.UsageEvent{
			Kind:             "usage",
			EventID:          eventID,
			SessionID:        sessionID,
			Timestamp:        tsStr,
			Provider:         cr.cfg.Provider,
			Model:            rec.Model,
			InputTokens:      rec.InputTokens,
			OutputTokens:     rec.OutputTokens,
			CacheReadTokens:  rec.CacheReadTokens,
			CacheWriteTokens: rec.CacheWriteTokens,
			CostUSD:          costUSD,
			IsSubagent:       false,
			ClientVersion:    config.ClientVersion,
		}

		s.captured.Add(1)
		if s.localStore != nil {
			_ = s.localStore.Append(localstore.Record{
				EventID:          ev.EventID,
				Timestamp:        ev.Timestamp,
				Route:            cr.cfg.Name,
				Provider:         ev.Provider,
				Model:            ev.Model,
				InputTokens:      ev.InputTokens,
				OutputTokens:     ev.OutputTokens,
				CacheReadTokens:  ev.CacheReadTokens,
				CacheWriteTokens: ev.CacheWriteTokens,
				CostUSD:          ev.CostUSD,
				CostEstimated:    estimated,
			})
		}
		if s.reporter != nil {
			s.reporter.Report(ev)
		}
		if s.verbose {
			s.logger.Printf("captured route=%s provider=%s model=%s in=%d out=%d cache_r=%d cache_w=%d cost=%.6f",
				cr.cfg.Name, ev.Provider, ev.Model, ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheWriteTokens, ev.CostUSD)
		}
	}
}

func (s *Server) resolveCost(route config.Route, rec extract.UsageRecord) (float64, bool) {
	if route.Local {
		return 0, false
	}
	if rec.ReportedCostUSD != nil {
		return *rec.ReportedCostUSD, false
	}
	if s.pricing != nil {
		if est, ok := s.pricing.Estimate(rec.Model, rec.InputTokens, rec.OutputTokens, rec.CacheReadTokens, rec.CacheWriteTokens); ok {
			return est, true
		}
	}
	return 0, false
}

func deriveEventID(provider string, rec extract.UsageRecord, headerReqID string, idx int) string {
	id := rec.ResponseID
	if id == "" {
		id = headerReqID
	}
	if id != "" {
		return ingest.NewUUIDv5(fmt.Sprintf("%s|%s|%s|%d", provider, id, rec.Model, idx))
	}
	return ingest.NewUUIDv4()
}

func (s *Server) deriveSessionID(r *http.Request, routeName string) string {
	if sid := firstNonEmpty(
		r.Header.Get("x-claude-code-session-id"),
		r.Header.Get("session_id"),
		r.Header.Get("x-session-id"),
	); sid != "" {
		mac := hmac.New(sha256.New, s.sessionKey[:])
		_, _ = mac.Write([]byte(sid))
		return "tmproxy:v2:" + hex.EncodeToString(mac.Sum(nil))
	}
	return "tmproxy:v2:route:" + routeName
}

// authorizedControl reports whether the request carries exactly one valid control token.
func (s *Server) authorizedControl(r *http.Request) bool {
	vals := r.Header.Values(controlHeader)
	if len(vals) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(vals[0]), []byte(s.controlToken)) == 1
}

func validNonce(n string) bool {
	if len(n) != 64 {
		return false
	}
	for i := range len(n) {
		c := n[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_tmproxy/health" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		vals := r.Header.Values(challengeHeader)
		if len(vals) != 1 || !validNonce(vals[0]) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_challenge"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": config.Version,
			"pid":     os.Getpid(),
			"proof":   s.localKey.Proof(vals[0]),
		})
		return
	}
	// Every other control request is authorized before any side effect.
	if !s.authorizedControl(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	switch r.URL.Path {
	case "/_tmproxy/status":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		writeJSON(w, http.StatusOK, s.Status())
	case "/_tmproxy/pause", "/_tmproxy/resume":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		paused := r.URL.Path == "/_tmproxy/pause"
		if err := s.setPaused(paused); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "desktop_pause_persistence_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": paused})
	case "/_tmproxy/flush":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		var flushErr string
		if s.reporter != nil {
			if err := s.reporter.Flush(r.Context(), true); err != nil {
				flushErr = err.Error()
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    flushErr == "",
			"error": flushErr,
			"stats": s.Status().Ingest,
		})
	case "/_tmproxy/shutdown":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "shutting_down": true})
		if s.onShutdownReq != nil {
			go s.onShutdownReq()
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

// SetPaused toggles reporting pause state and persists it to config.json if Paths are set.
func (s *Server) SetPaused(paused bool) {
	_ = s.setPaused(paused)
}

func (s *Server) setPaused(paused bool) error {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	var desktopErr error
	if s.onPauseChange != nil {
		desktopErr = s.onPauseChange(paused)
		if desktopErr != nil {
			s.logger.Print("desktop: could not persist reporting pause state")
			if !paused {
				return desktopErr
			}
		}
	}
	s.paused.Store(paused)
	if s.paths.ConfigDir != "" {
		if diskCfg, err := config.LoadFile(s.paths); err == nil {
			diskCfg.Paused = paused
			_ = config.Save(s.paths, diskCfg)
		}
	}
	return desktopErr
}

// Status returns the current runtime status snapshot.
func (s *Server) Status() StatusSnapshot {
	s.mu.RLock()
	addr := s.listenAddr
	s.mu.RUnlock()

	var ingStats ingest.Stats
	if s.reporter != nil {
		ingStats = s.reporter.Stats()
	}

	keyStatus := "not configured"
	if s.cfg.APIKey != "" {
		keyStatus = "configured"
	}
	var ingestOrigin string
	if ingest.ValidateURL(s.cfg.IngestURL) == nil {
		u, _ := url.Parse(s.cfg.IngestURL)
		ingestOrigin = u.Scheme + "://" + u.Host
	}
	var desktop *DesktopStatus
	if s.desktopStatus != nil {
		desktop = s.desktopStatus()
	}
	return StatusSnapshot{
		Version:      config.Version,
		PID:          os.Getpid(),
		ListenAddr:   addr,
		IngestURL:    ingestOrigin,
		APIKeyMasked: keyStatus,
		HasAPIKey:    s.cfg.APIKey != "",
		Paused:       s.paused.Load(),
		UptimeSec:    int64(s.now().Sub(s.startedAt).Seconds()),
		Captured:     s.captured.Load(),
		SkippedPause: s.skippedPause.Load(),
		Routes:       s.cfg.SortedRouteNames(),
		Ingest:       ingStats,
		Desktop:      desktop,
	}
}

// splitRoutePath splits an escaped path (after the local capability prefix)
// into the lower-cased route name and the still-escaped remainder.
func splitRoutePath(escaped string) (route string, rest string, ok bool) {
	trimmed := strings.TrimPrefix(escaped, "/")
	if trimmed == "" {
		return "", "", false
	}
	if idx := strings.IndexByte(trimmed, '/'); idx >= 0 {
		return strings.ToLower(trimmed[:idx]), trimmed[idx:], true
	}
	return strings.ToLower(trimmed), "/", true
}

// decodeSubPath validates an escaped upstream sub-path and returns its decoded
// form. Dot segments (literal or escaped) are rejected so the request cannot
// climb out of the route's upstream base path.
func decodeSubPath(escaped string) (string, error) {
	for _, seg := range strings.Split(escaped, "/") {
		u, err := url.PathUnescape(seg)
		if err != nil {
			return "", err
		}
		if u == "." || u == ".." {
			return "", errors.New("dot segment")
		}
	}
	return url.PathUnescape(escaped)
}

// joinUpstreamURL appends the escaped sub-path to the upstream base while
// preserving escaped segments (e.g. %2F) verbatim.
func joinUpstreamURL(base *url.URL, escapedSub, decodedSub, rawQuery string) *url.URL {
	out := *base
	baseEscaped := strings.TrimRight(base.EscapedPath(), "/")
	out.Path = strings.TrimRight(base.Path, "/") + decodedSub
	out.RawPath = baseEscaped + escapedSub
	out.RawQuery = rawQuery
	return &out
}

func isLoopbackHost(host string) bool {
	h := host
	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		h = splitHost
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func hasBrowserHeaders(h http.Header) bool {
	const prefix = "Sec-Fetch-"
	for name := range h {
		if strings.EqualFold(name, "Origin") ||
			(len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix)) {
			return true
		}
	}
	return false
}

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHop removes hop-by-hop headers, including every header named by
// any token of any Connection header line.
func stripHopByHop(h http.Header) {
	for _, c := range h.Values("Connection") {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}

// stripLocalAuth removes local capability/control material and referrer data so
// it can never be forwarded to a provider.
func stripLocalAuth(h http.Header) {
	h.Del("Referer")
	h.Del(controlHeader)
	h.Del(challengeHeader)
}

// setSecurityHeaders applies response hardening; Set (not Add) so upstream values cannot weaken it.
func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

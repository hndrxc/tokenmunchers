package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
)

var testKey = func() localauth.Key {
	var k localauth.Key
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}()

var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// newAuthProxy starts an upstream running up and a proxy fronting it. The
// returned counter records how many requests reached the upstream.
func newAuthProxy(t *testing.T, up http.HandlerFunc, onShutdown func()) (*Server, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		up(w, r)
	}))
	t.Cleanup(upSrv.Close)
	srv, err := New(Options{
		LocalKey:      testKey,
		OnShutdownReq: onShutdown,
		Config: config.Config{
			ListenAddr: "127.0.0.1:0",
			Routes: map[string]config.Route{
				"anthropic": {Upstream: upSrv.URL, Provider: "anthropic", Format: config.FormatAnthropic},
				"scoped":    {Upstream: upSrv.URL + "/api", Provider: "anthropic", Format: config.FormatAnthropic},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ps := httptest.NewServer(srv)
	t.Cleanup(ps.Close)
	return srv, ps, &hits
}

func okUpstream(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func doReq(t *testing.T, method, url string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestNewRequiresLocalKey(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("accepted a zero local key")
	}
}

func TestControlAuthorization(t *testing.T) {
	var shutdowns atomic.Int32
	srv, ps, _ := newAuthProxy(t, okUpstream, func() { shutdowns.Add(1) })

	for name, hdr := range map[string]map[string]string{
		"missing": {},
		"wrong":   {controlHeader: strings.Repeat("0", len(testKey.ControlToken()))},
		"prefix":  {controlHeader: testKey.ControlToken()[:10]},
		"route":   {controlHeader: testKey.RoutePrefix()},
	} {
		for _, action := range []string{"pause", "shutdown", "flush", "resume"} {
			resp := doReq(t, http.MethodPost, ps.URL+"/_tmproxy/"+action, nil, hdr)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s/%s: status = %d, want 401", name, action, resp.StatusCode)
			}
		}
		resp := doReq(t, http.MethodGet, ps.URL+"/_tmproxy/status", nil, hdr)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s status: %d, want 401", name, resp.StatusCode)
		}
	}
	if srv.paused.Load() {
		t.Fatal("unauthorized pause changed state")
	}

	// Duplicate credential headers are ambiguous and rejected.
	req, _ := http.NewRequest(http.MethodPost, ps.URL+"/_tmproxy/pause", nil)
	req.Header.Add(controlHeader, testKey.ControlToken())
	req.Header.Add(controlHeader, testKey.ControlToken())
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || srv.paused.Load() {
		t.Fatalf("duplicate control header accepted: %d", resp.StatusCode)
	}

	auth := map[string]string{controlHeader: testKey.ControlToken()}
	resp = doReq(t, http.MethodPost, ps.URL+"/_tmproxy/pause", nil, auth)
	if resp.StatusCode != http.StatusOK || !srv.paused.Load() {
		t.Fatalf("authorized pause failed: %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodGet, ps.URL+"/_tmproxy/status", nil, auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized status: %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodPost, ps.URL+"/_tmproxy/resume", nil, auth)
	if resp.StatusCode != http.StatusOK || srv.paused.Load() {
		t.Fatalf("authorized resume failed: %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodGet, ps.URL+"/_tmproxy/pause", nil, auth)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("authorized wrong method: %d", resp.StatusCode)
	}
	if shutdowns.Load() != 0 {
		t.Fatal("unauthorized shutdown ran")
	}
	resp = doReq(t, http.MethodPost, ps.URL+"/_tmproxy/shutdown", nil, auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized shutdown: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for shutdowns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if shutdowns.Load() != 1 {
		t.Fatal("authorized shutdown did not run")
	}
}

func TestHealthChallenge(t *testing.T) {
	_, ps, _ := newAuthProxy(t, okUpstream, nil)
	nonce := strings.Repeat("ab", 32)

	// Health needs no control credential and never echoes it.
	resp := doReq(t, http.MethodGet, ps.URL+"/_tmproxy/health", nil, map[string]string{challengeHeader: nonce})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d %s", resp.StatusCode, body)
	}
	var got struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		PID     int    `json:"pid"`
		Proof   string `json:"proof"`
	}
	if err := json.Unmarshal(body, &got); err != nil || !got.OK || got.PID == 0 || got.Version == "" {
		t.Fatalf("unexpected health body: %s", body)
	}
	if want := testKey.Proof(nonce); want == "" || got.Proof != want {
		t.Fatalf("proof = %q, want %q", got.Proof, want)
	}
	if strings.Contains(string(body), testKey.ControlToken()) || strings.Contains(string(body), testKey.RoutePrefix()) {
		t.Fatal("health leaked local credential")
	}

	for name, n := range map[string]string{
		"missing":   "",
		"short":     "abcd",
		"uppercase": strings.Repeat("AB", 32),
		"non-hex":   strings.Repeat("zz", 32),
		"long":      strings.Repeat("ab", 33),
	} {
		hdr := map[string]string{}
		if n != "" {
			hdr[challengeHeader] = n
		}
		resp := doReq(t, http.MethodGet, ps.URL+"/_tmproxy/health", nil, hdr)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s nonce: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestCapabilityRequired(t *testing.T) {
	_, ps, hits := newAuthProxy(t, okUpstream, nil)
	prefix := testKey.RoutePrefix()
	escapedTM := strings.Replace(prefix, "/_tm/", "/%5Ftm/", 1)
	for name, path := range map[string]string{
		"legacy":       "/anthropic/v1/messages",
		"wrong":        "/_tm/" + strings.Repeat("0", len(prefix)-len("/_tm/")) + "/anthropic/v1/messages",
		"escaped":      escapedTM + "/anthropic/v1/messages",
		"extended":     prefix + "x/anthropic/v1/messages",
		"truncated":    prefix[:len(prefix)-1] + "/anthropic/v1/messages",
		"double-slash": "/" + prefix + "/anthropic/v1/messages",
	} {
		resp := doReq(t, http.MethodPost, ps.URL+path, strings.NewReader("{}"), nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, resp.StatusCode)
		}
		if body, _ := io.ReadAll(resp.Body); bytes.Contains(body, []byte("anthropic")) {
			t.Errorf("%s: unauthenticated response reveals routes: %s", name, body)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("unauthorized request reached upstream (%d hits)", hits.Load())
	}

	// With the capability, the same request is forwarded.
	resp := doReq(t, http.MethodPost, ps.URL+prefix+"/anthropic/v1/messages", strings.NewReader("{}"), nil)
	if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("authorized request: status=%d hits=%d", resp.StatusCode, hits.Load())
	}
	// Websocket rejection also requires the capability.
	resp = doReq(t, http.MethodGet, ps.URL+"/anthropic/v1/messages", nil, map[string]string{"Upgrade": "websocket"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unauthenticated websocket: %d", resp.StatusCode)
	}
}

func TestForwardingStripsLocalMaterial(t *testing.T) {
	type seen struct {
		escapedPath, requestURI, query string
		header                         http.Header
	}
	got := make(chan seen, 4)
	_, ps, _ := newAuthProxy(t, func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.URL.EscapedPath(), r.RequestURI, r.URL.RawQuery, r.Header.Clone()}
	}, nil)
	prefix := testKey.RoutePrefix()

	hdr := map[string]string{
		"Referer":          ps.URL + prefix + "/anthropic/v1/messages",
		controlHeader:      testKey.ControlToken(),
		challengeHeader:    strings.Repeat("ab", 32),
		"Authorization":    "Bearer provider-secret",
		"X-Hop-Private":    "drop-me",
		"Connection":       "X-Hop-Private",
		"X-Other-Hop":      "drop-me-too",
		"Proxy-Connection": "keep-alive",
		"X-End-To-End":     "keep-me",
	}
	req, _ := http.NewRequest(http.MethodGet, ps.URL+prefix+"/anthropic/v1/a%2Fb/c%20d?x=1&y=%2F", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Add("Connection", "X-Other-Hop")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	s := <-got
	if s.escapedPath != "/v1/a%2Fb/c%20d" || s.query != "x=1&y=%2F" {
		t.Fatalf("upstream path/query = %q ? %q", s.escapedPath, s.query)
	}
	if strings.Contains(s.requestURI, "_tm") || strings.Contains(s.requestURI, prefix) {
		t.Fatalf("capability reached upstream: %q", s.requestURI)
	}
	for _, name := range []string{"Referer", controlHeader, challengeHeader, "X-Hop-Private", "X-Other-Hop", "Proxy-Connection"} {
		if v := s.header.Values(name); len(v) != 0 {
			t.Errorf("header %s forwarded upstream: %q", name, v)
		}
	}
	if s.header.Get("Authorization") != "Bearer provider-secret" || s.header.Get("X-End-To-End") != "keep-me" {
		t.Fatalf("end-to-end headers not preserved: %v", s.header)
	}
	for k, vs := range s.header {
		for _, v := range vs {
			if strings.Contains(v, prefix) || strings.Contains(v, testKey.ControlToken()) {
				t.Fatalf("local credential in upstream header %s", k)
			}
		}
	}

	// Escaped segments are preserved beneath an upstream base path as well.
	resp = doReq(t, http.MethodGet, ps.URL+prefix+"/scoped/v1/x%2Fy", nil, nil)
	if s = <-got; s.escapedPath != "/api/v1/x%2Fy" {
		t.Fatalf("scoped upstream path = %q", s.escapedPath)
	}
	_ = resp
}

func TestDotSegmentsRejected(t *testing.T) {
	_, ps, hits := newAuthProxy(t, okUpstream, nil)
	prefix := testKey.RoutePrefix()
	for _, p := range []string{"/scoped/../admin", "/scoped/%2e%2e/admin", "/scoped/./x", "/scoped/%2E/x", "/scoped/a/%2e%2E"} {
		resp := doReq(t, http.MethodGet, ps.URL+prefix+p, nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", p, resp.StatusCode)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("dot-segment request reached upstream")
	}
}

func TestResponseHardeningAndRedirectBlock(t *testing.T) {
	_, ps, _ := newAuthProxy(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", "https://evil.example/")
			w.Header().Set("Set-Cookie", "a=b")
			w.WriteHeader(http.StatusFound)
		case "/redirect-nolocation":
			w.WriteHeader(http.StatusFound)
		case "/notmodified":
			w.Header().Set("Location", "https://provider.example/x")
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("Refresh", "0; url=https://evil.example/")
			w.Header().Set("Referrer-Policy", "unsafe-url")
			w.Header().Set("Content-Security-Policy", "default-src *")
			w.Header().Set("X-Provider", "kept")
			_, _ = w.Write([]byte("hello"))
		}
	}, nil)
	prefix := testKey.RoutePrefix()

	resp := doReq(t, http.MethodGet, ps.URL+prefix+"/anthropic/redirect", nil, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" ||
		resp.Header.Get("Set-Cookie") != "" || strings.Contains(string(body), "evil.example") {
		t.Fatalf("redirect not blocked: %d %v %s", resp.StatusCode, resp.Header, body)
	}

	resp = doReq(t, http.MethodGet, ps.URL+prefix+"/anthropic/redirect-nolocation", nil, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("3xx without Location should pass through, got %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodGet, ps.URL+prefix+"/anthropic/notmodified", nil, nil)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("304 must be preserved, got %d", resp.StatusCode)
	}

	resp = doReq(t, http.MethodGet, ps.URL+prefix+"/anthropic/ok", nil, nil)
	body, _ = io.ReadAll(resp.Body)
	if string(body) != "hello" || resp.Header.Get("X-Provider") != "kept" {
		t.Fatalf("ordinary response altered: %q %v", body, resp.Header)
	}
	if v := resp.Header.Values("Referrer-Policy"); len(v) != 1 || v[0] != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", v)
	}
	if v := resp.Header.Values("Content-Security-Policy"); len(v) != 1 || !strings.Contains(v[0], "sandbox") || strings.Contains(v[0], "*") {
		t.Fatalf("Content-Security-Policy = %q", v)
	}
	if resp.Header.Get("Refresh") != "" {
		t.Fatal("Refresh header forwarded")
	}
	// Error responses carry the hardening headers too.
	resp = doReq(t, http.MethodGet, ps.URL+"/anthropic/ok", nil, nil)
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("error response missing Referrer-Policy")
	}
}

func TestRequestBodyPassthrough(t *testing.T) {
	type seen struct {
		contentLength int64
		chunked       bool
		body          []byte
	}
	got := make(chan seen, 4)
	_, ps, _ := newAuthProxy(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.ContentLength, len(r.TransferEncoding) > 0, b}
		_, _ = w.Write(b)
	}, nil)
	url := ps.URL + testKey.RoutePrefix() + "/anthropic/v1/messages"

	big := bytes.Repeat([]byte("0123456789abcdef"), (MaxRequestModelPeek*3)/16+7)
	for name, tc := range map[string]struct {
		body    io.Reader
		wantLen int64
		want    []byte
	}{
		"empty":   {http.NoBody, 0, nil},
		"small":   {strings.NewReader(`{"model":"m"}`), 13, []byte(`{"model":"m"}`)},
		"large":   {bytes.NewReader(big), int64(len(big)), big},
		"chunked": {io.NopCloser(bytes.NewReader(big)), -1, big},
	} {
		req, _ := http.NewRequest(http.MethodPost, url, tc.body)
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		echoed, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s := <-got
		if !bytes.Equal(s.body, tc.want) || !bytes.Equal(echoed, tc.want) {
			t.Errorf("%s: body mismatch (upstream %d bytes, echoed %d, want %d)", name, len(s.body), len(echoed), len(tc.want))
		}
		if tc.wantLen >= 0 && s.contentLength != tc.wantLen {
			t.Errorf("%s: upstream content length = %d, want %d", name, s.contentLength, tc.wantLen)
		}
		if tc.wantLen < 0 && !s.chunked {
			t.Errorf("%s: expected chunked upstream request", name)
		}
	}
}

func TestUpstreamBodyFailureAbortsDownstream(t *testing.T) {
	const sse = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"input_tokens\":1}}}\n\n"
	_, ps, _ := newAuthProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/fixed") {
			w.Header().Set("Content-Length", "100000")
		}
		w.WriteHeader(http.StatusOK)
		if !strings.HasSuffix(r.URL.Path, "/complete") {
			_, _ = w.Write([]byte(sse))
			w.(http.Flusher).Flush()
			// Tear the upstream connection down mid-body without a clean end.
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write([]byte(sse))
	}, nil)
	prefix := testKey.RoutePrefix()

	for _, tc := range []struct {
		name, path, method string
		wantErr            bool
	}{
		{"complete", "/anthropic/v1/messages/complete", http.MethodPost, false},
		{"chunked post tapped", "/anthropic/v1/messages/chunked", http.MethodPost, true},
		{"chunked get untapped", "/anthropic/v1/messages/chunked", http.MethodGet, true},
		{"fixed length post", "/anthropic/v1/messages/fixed", http.MethodPost, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.method == http.MethodPost {
				body = strings.NewReader(`{"model":"x"}`)
			}
			req, _ := http.NewRequest(tc.method, ps.URL+prefix+tc.path, body)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if tc.wantErr && err == nil {
				t.Fatalf("truncated upstream body surfaced as a clean response (%d bytes)", len(data))
			}
			if !tc.wantErr && (err != nil || string(data) != sse) {
				t.Fatalf("complete response failed: %v %q", err, data)
			}
		})
	}
}

func TestUpstreamFailureAbortsHandler(t *testing.T) {
	// The handler must abort (not return normally) after an upstream read failure.
	srv, err := New(Options{LocalKey: testKey, Config: config.Config{
		Routes: map[string]config.Route{
			"anthropic": {Upstream: "http://127.0.0.1:1", Provider: "anthropic", Format: config.FormatAnthropic},
		},
	}, UpstreamHTTP: failingBodyTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+testKey.RoutePrefix()+"/anthropic/v1/x", nil)
	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", r)
		}
	}()
	srv.ServeHTTP(rec, req)
	t.Fatal("handler returned normally after upstream body failure")
}

type failingBodyTransport struct{}

func (failingBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(io.MultiReader(strings.NewReader("partial"), errReader{})),
	}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestOnReadyAfterBind(t *testing.T) {
	ready := make(chan string, 1)
	var srv *Server
	srv, err := New(Options{
		LocalKey: testKey,
		Config:   config.Config{ListenAddr: "127.0.0.1:0"},
		OnReady:  func() { ready <- srv.Status().ListenAddr },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	select {
	case addr := <-ready:
		if strings.HasSuffix(addr, ":0") {
			t.Fatalf("OnReady ran before bind: %q", addr)
		}
		resp := doReq(t, http.MethodGet, "http://"+addr+"/_tmproxy/health", nil, map[string]string{challengeHeader: strings.Repeat("ab", 32)})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("health after ready: %d", resp.StatusCode)
		}
	case err := <-done:
		t.Fatalf("server exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady not called")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timed out")
	}
}

func TestOnReadyNotCalledWhenBindFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var called atomic.Bool
	srv, err := New(Options{
		LocalKey: testKey,
		Config:   config.Config{ListenAddr: ln.Addr().String()},
		OnReady:  func() { called.Store(true) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ListenAndServe(context.Background()); err == nil {
		t.Fatal("expected bind failure")
	}
	if called.Load() {
		t.Fatal("OnReady called despite bind failure")
	}
}

func TestStripHopByHop(t *testing.T) {
	h := http.Header{}
	h.Add("Connection", "X-A, keep-alive")
	h.Add("Connection", "x-b")
	h.Set("X-A", "1")
	h.Set("X-B", "2")
	h.Set("X-C", "3")
	h.Set("Trailer", "X-Sum")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Te", "trailers")
	stripHopByHop(h)
	if len(h) != 1 || h.Get("X-C") != "3" {
		t.Fatalf("unexpected headers after strip: %v", h)
	}
}

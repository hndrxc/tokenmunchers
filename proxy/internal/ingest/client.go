package ingest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MaxBatchEvents   = 100
	MaxBodyBytes     = 60 * 1024 // server limit is 64 KiB; leave safety margin
	MaxEventAge      = 6*24*time.Hour + 12*time.Hour // ingest rejects > 7 days
	DefaultTimeout   = 10 * time.Second
	BaseBackoff      = 5 * time.Second
	MaxBackoff       = 15 * time.Minute
	StaleFlushWindow = 5 * time.Minute
)

// ValidateURL reports whether raw is an acceptable telemetry destination:
// absolute https with a host and no userinfo, query, or fragment; cleartext
// http is allowed only for numeric loopback IPs. The error text never echoes raw.
// NewReporter applies the same policy and refuses all network use on failure.
func ValidateURL(raw string) error {
	_, err := validateIngestURL(raw)
	return err
}

// LegacySessionID replaces the session ID of queued events written before the
// queue-only safety marker existed (their session IDs may be raw client data).
const LegacySessionID = "tmproxy:v2:legacy"

// queueMarker is stored only in the on-disk queue (never sent on the wire) to
// attest that an entry was written by a proxy whose session IDs are already
// opaque. Entries lacking it are treated as legacy and redacted on flush.
const queueMarker = 2

// queueEntry is the JSONL queue line shape: the wire event plus the marker.
type queueEntry struct {
	UsageEvent
	Marker int `json:"tm_queue,omitempty"`
}

// Fixed UUIDv5 namespace dedicated to tmproxy event IDs.
var eventNamespace = [16]byte{
	0x91, 0x7b, 0x2c, 0x4e, 0x8a, 0x10, 0x4d, 0x3b,
	0xa6, 0x21, 0x5c, 0x90, 0x7e, 0x3f, 0x18, 0x42,
}

// UsageEvent matches the strict field allowlist of POST /functions/v1/ingest.
type UsageEvent struct {
	Kind             string  `json:"kind"`
	EventID          string  `json:"event_id"`
	SessionID        string  `json:"session_id"`
	Timestamp        string  `json:"ts"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	IsSubagent       bool    `json:"is_subagent"`
	ClientVersion    string  `json:"client_version,omitempty"`
}

// SendResult classifies the outcome of an ingest HTTP request.
type SendResult int

const (
	SendOK SendResult = iota
	SendRetry
	SendUnauthorized
	SendRejected
)

// Stats snapshots reporter health for status inspection.
type Stats struct {
	Delivered int64  `json:"delivered"`
	Queued    int    `json:"queued"`
	Dropped   int64  `json:"dropped"`
	LastError string `json:"last_error,omitempty"`
	Retained  int    `json:"retained,omitempty"` // claimed queue files kept on disk awaiting retry
}

// Options configures the ingest Reporter.
type Options struct {
	QueuePath  string
	IngestURL  string
	APIKey     string
	HTTPClient *http.Client
	Now        func() time.Time
}

// Reporter delivers UsageEvents to TokenMunchers /functions/v1/ingest with
// an on-disk JSONL retry queue and exponential backoff.
type Reporter struct {
	queuePath string
	ingestURL string
	urlErr    error // non-nil when the destination failed policy validation; blocks all network use
	apiKey    string
	client    *http.Client
	now       func() time.Time

	mu          sync.Mutex
	flushing    bool
	failures    int
	nextFlushAt time.Time
	delivered   int64
	dropped     int64
	lastError   string
	retained    map[string]struct{} // claimed .flushing files kept on disk for retry

	notifyCh chan struct{}
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewReporter creates a Reporter. Call Start() to enable background delivery/retry.
func NewReporter(opts Options) *Reporter {
	client := noRedirectClient(opts.HTTPClient)
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	ingestURL, urlErr := validateIngestURL(opts.IngestURL)
	return &Reporter{
		queuePath: opts.QueuePath,
		ingestURL: ingestURL,
		urlErr:    urlErr,
		apiKey:    opts.APIKey,
		client:    client,
		now:       nowFn,
		notifyCh:  make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
	}
}

// Start launches the background retry loop.
func (r *Reporter) Start() {
	r.wg.Add(1)
	go r.loop()
}

// Stop stops the background loop and performs a final best-effort flush.
func (r *Reporter) Stop(ctx context.Context) {
	close(r.stopCh)
	r.wg.Wait()
	_ = r.Flush(ctx, true)
}

func (r *Reporter) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-r.notifyCh:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = r.Flush(ctx, false)
			cancel()
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = r.Flush(ctx, false)
			cancel()
		}
	}
}

// Report sends an event asynchronously, falling back to the on-disk queue on failure.
// Never blocks the caller for network I/O.
func (r *Reporter) Report(ev UsageEvent) {
	ev = sanitizeEvent(ev)
	if ev.InputTokens+ev.OutputTokens+ev.CacheReadTokens+ev.CacheWriteTokens <= 0 {
		return
	}
	// Persist first or attempt fast-path delivery. Writing to queue and waking
	// the flusher guarantees durability even if the process crashes mid-flight,
	// while still delivering within milliseconds!
	if err := r.Enqueue([]UsageEvent{ev}); err != nil {
		r.setError("enqueue failed")
		// Fallback: attempt direct send in background
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
			defer cancel()
			_, _ = r.Send(ctx, []UsageEvent{ev})
		}()
		return
	}
	select {
	case r.notifyCh <- struct{}{}:
	default:
	}
}

// Stats returns a snapshot of delivery counters and queue size.
func (r *Reporter) Stats() Stats {
	r.mu.Lock()
	s := Stats{
		Delivered: r.delivered,
		Dropped:   r.dropped,
		LastError: r.lastError,
		Retained:  len(r.retained),
	}
	r.mu.Unlock()
	s.Queued = r.QueueSize()
	return s
}

// Enqueue appends events to the local JSONL queue file.
func (r *Reporter) Enqueue(events []UsageEvent) error {
	if len(events) == 0 || r.queuePath == "" {
		return nil
	}
	buf := encodeEntries(events)
	if len(buf) == 0 {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(r.queuePath), 0o700); err != nil {
		return errQueueWrite
	}
	f, err := os.OpenFile(r.queuePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return errQueueWrite
	}
	defer f.Close()
	var before int64 = -1
	if st, err := f.Stat(); err == nil {
		before = st.Size()
	}
	if _, err := f.Write(buf); err != nil {
		// A short write would leave a torn line that corrupts the next
		// appended entry; roll back to the previous size when possible.
		if before >= 0 {
			_ = f.Truncate(before)
		}
		return errQueueWrite
	}
	return nil
}

// encodeEntries serializes events as queue JSONL lines carrying the safety marker.
func encodeEntries(events []UsageEvent) []byte {
	var buf bytes.Buffer
	for _, ev := range events {
		b, err := json.Marshal(queueEntry{UsageEvent: ev, Marker: queueMarker})
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// errQueueWrite hides filesystem paths carried by the underlying os errors.
var errQueueWrite = errors.New("queue write failed")

// QueueSize counts pending lines in the queue file.
func (r *Reporter) QueueSize() int {
	if r.queuePath == "" {
		return 0
	}
	f, err := os.Open(r.queuePath)
	if err != nil {
		return 0
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 16*1024), 512*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) > 0 {
			count++
		}
	}
	return count
}

// Flush drains queued events to /functions/v1/ingest in batches.
func (r *Reporter) Flush(ctx context.Context, force bool) error {
	r.mu.Lock()
	if r.flushing {
		r.mu.Unlock()
		return nil
	}
	if r.urlErr != nil {
		r.lastError = r.urlErr.Error()
		r.mu.Unlock()
		return r.urlErr
	}
	if r.apiKey == "" {
		r.lastError = "API key not configured (run `munch login`)"
		r.mu.Unlock()
		return errors.New(r.lastError)
	}
	if !force && r.now().Before(r.nextFlushAt) {
		r.mu.Unlock()
		return nil
	}
	r.flushing = true
	claimed := r.claimQueueFilesLocked()
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.flushing = false
		r.mu.Unlock()
	}()

	for i, file := range claimed {
		if ok := r.flushFile(ctx, file); !ok {
			// Files not yet attempted stay claimed on disk; track them so the
			// next backoff-gated flush retries them instead of waiting for the
			// stale-claim window.
			r.mu.Lock()
			if r.retained == nil {
				r.retained = make(map[string]struct{})
			}
			for _, rest := range claimed[i+1:] {
				r.retained[rest] = struct{}{}
			}
			r.mu.Unlock()
			return errors.New(r.Stats().LastError)
		}
	}
	return nil
}

func (r *Reporter) claimQueueFilesLocked() []string {
	if r.queuePath == "" {
		return nil
	}
	var claimed []string
	seen := make(map[string]struct{})

	// Claims this process kept after a failed flush are retried first (they
	// hold the oldest events), regardless of the stale-claim window.
	retained := make([]string, 0, len(r.retained))
	for f := range r.retained {
		retained = append(retained, f)
	}
	sort.Strings(retained)
	for _, f := range retained {
		if _, err := os.Stat(f); err != nil {
			delete(r.retained, f)
			continue
		}
		seen[f] = struct{}{}
		claimed = append(claimed, f)
	}

	if _, err := os.Stat(r.queuePath); err == nil {
		// Never rename over an existing claim (would destroy retained events).
		stamp := r.now().UnixNano()
		target := fmt.Sprintf("%s.%d.%d.flushing", r.queuePath, os.Getpid(), stamp)
		for {
			if _, serr := os.Lstat(target); serr != nil {
				break
			}
			stamp++
			target = fmt.Sprintf("%s.%d.%d.flushing", r.queuePath, os.Getpid(), stamp)
		}
		if err := os.Rename(r.queuePath, target); err == nil {
			seen[target] = struct{}{}
			claimed = append(claimed, target)
		}
	}

	dir := filepath.Dir(r.queuePath)
	base := filepath.Base(r.queuePath) + "."
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, base) || !strings.HasSuffix(name, ".flushing") {
				continue
			}
			full := filepath.Join(dir, name)
			if _, already := seen[full]; already {
				continue
			}
			if info, err := e.Info(); err == nil && r.now().Sub(info.ModTime()) > StaleFlushWindow {
				seen[full] = struct{}{}
				claimed = append(claimed, full)
			}
		}
	}
	return claimed
}

// flushFile delivers one claimed queue file. The claimed file is the durable
// copy of its events: it is deleted only after every event was delivered or
// safely re-persisted. On any failure it is kept (and tracked for retry under
// the normal backoff). Events carry durable IDs, so a retry after partial
// delivery at worst re-sends events the server de-duplicates.
func (r *Reporter) flushFile(ctx context.Context, file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.forgetRetained(file)
			return true
		}
		r.retainFailed(file, "queue read failed; will retry")
		return false
	}

	now := r.now()
	var events []UsageEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 16*1024), 512*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry queueEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			r.incDropped(1)
			continue
		}
		ev := entry.UsageEvent
		ts, err := time.Parse(time.RFC3339Nano, ev.Timestamp)
		if err != nil || now.Sub(ts) > MaxEventAge {
			r.incDropped(1)
			continue
		}
		if entry.Marker != queueMarker {
			// Entry predates the safe-session queue format and may carry raw
			// session header data; never upload it.
			ev.SessionID = LegacySessionID
		}
		events = append(events, ev)
	}

	for len(events) > 0 {
		batch := takeBatch(events)
		res, err := r.Send(ctx, batch)
		if err != nil {
			r.setError(err.Error())
		}
		if res == SendRetry || res == SendUnauthorized {
			if r.Enqueue(events) == nil {
				if r.removeClaimed(file) != nil {
					r.retainFailed(file, "queue cleanup failed; will retry")
					return false
				}
				r.forgetRetained(file)
				r.backoff()
				return false
			}
			// The queue is unwritable: keep the claimed copy. Shrink it to the
			// undelivered, already-redacted events when possible so delivered or
			// dropped lines are not repeated; otherwise the original stays as is.
			_ = rewriteClaimed(file, events)
			r.retainFailed(file, "queue unwritable; events kept for retry")
			return false
		}
		events = events[len(batch):]
	}

	if err := r.removeClaimed(file); err != nil {
		// Everything was delivered but the file could not be cleared; keep it
		// tracked so cleanup is retried instead of silently re-sending forever.
		r.retainFailed(file, "queue cleanup failed; will retry")
		return false
	}
	r.forgetRetained(file)
	r.resetBackoff()
	return true
}

// removeClaimed deletes a claimed file, falling back to truncation so a file
// that cannot be unlinked cannot cause re-delivery of its events.
func (r *Reporter) removeClaimed(file string) error {
	err := os.Remove(file)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if terr := os.Truncate(file, 0); terr != nil {
		return errQueueWrite
	}
	return nil
}

// rewriteClaimed atomically replaces file with the given events. On failure
// the original file is untouched.
func rewriteClaimed(file string, events []UsageEvent) error {
	buf := encodeEntries(events)
	if len(buf) == 0 {
		return errQueueWrite
	}
	tmp := file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return errQueueWrite
	}
	_, werr := f.Write(buf)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return errQueueWrite
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return errQueueWrite
	}
	return nil
}

// retainFailed records that file must be retried, applies backoff, and sets a
// path-free diagnostic (appended to any delivery error already recorded).
func (r *Reporter) retainFailed(file, msg string) {
	r.mu.Lock()
	if r.retained == nil {
		r.retained = make(map[string]struct{})
	}
	r.retained[file] = struct{}{}
	if r.lastError == "" {
		r.lastError = msg
	} else {
		r.lastError += "; " + msg
	}
	r.mu.Unlock()
	r.backoff()
}

func (r *Reporter) forgetRetained(file string) {
	r.mu.Lock()
	delete(r.retained, file)
	r.mu.Unlock()
}

// takeBatch slices up to MaxBatchEvents that serialize within MaxBodyBytes.
func takeBatch(events []UsageEvent) []UsageEvent {
	limit := min(len(events), MaxBatchEvents)
	if limit <= 1 {
		return events[:limit]
	}
	// Estimate conservatively: {"events":[ ... ]}
	totalBytes := 16
	n := 0
	for i := 0; i < limit; i++ {
		b, err := json.Marshal(events[i])
		if err != nil {
			continue
		}
		if n > 0 && totalBytes+len(b)+1 > MaxBodyBytes {
			break
		}
		totalBytes += len(b) + 1
		n++
	}
	if n == 0 {
		return events[:1]
	}
	return events[:n]
}

// Send posts a batch of events to /functions/v1/ingest.
// Mirrors plugin/src/transport.ts: drops single invalid events on 400 index responses.
func (r *Reporter) Send(ctx context.Context, events []UsageEvent) (SendResult, error) {
	if len(events) == 0 {
		return SendOK, nil
	}
	if r.urlErr != nil {
		r.setError(r.urlErr.Error())
		return SendRejected, r.urlErr
	}
	if r.apiKey == "" {
		return SendUnauthorized, errors.New("API key not configured")
	}

	pending := make([]UsageEvent, len(events))
	copy(pending, events)

	for len(pending) > 0 {
		var payload any
		if len(pending) == 1 {
			payload = pending[0]
		} else {
			payload = struct {
				Events []UsageEvent `json:"events"`
			}{Events: pending}
		}
		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			r.setError(errEncode.Error())
			return SendRejected, errEncode
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ingestURL, bytes.NewReader(bodyBytes))
		if err != nil {
			r.setError(errBuildRequest.Error())
			return SendRejected, errBuildRequest
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+r.apiKey)

		resp, err := r.client.Do(req)
		if err != nil {
			// Transport errors embed the destination URL; expose only a coarse class.
			serr := transportError(ctx)
			r.setError(serr.Error())
			return SendRetry, serr
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			r.mu.Lock()
			r.delivered += int64(len(pending))
			r.lastError = ""
			r.mu.Unlock()
			return SendOK, nil
		}

		// Only the invalid-event index is ever read from the response; remote
		// text is never surfaced.
		var errBody struct {
			Index *int `json:"index"`
		}
		_ = json.Unmarshal(respBody, &errBody)
		serr := statusError(resp.StatusCode)
		r.setError(serr.Error())

		if resp.StatusCode == http.StatusUnauthorized {
			return SendUnauthorized, serr
		}
		if resp.StatusCode == http.StatusBadRequest && errBody.Index != nil && len(pending) > 1 {
			idx := *errBody.Index
			if idx >= 0 && idx < len(pending) {
				pending = append(pending[:idx], pending[idx+1:]...)
				r.incDropped(1)
				continue
			}
		}
		if resp.StatusCode == http.StatusRequestEntityTooLarge && len(pending) > 1 {
			mid := len(pending) / 2
			res1, err1 := r.Send(ctx, pending[:mid])
			if res1 != SendOK {
				return res1, err1
			}
			return r.Send(ctx, pending[mid:])
		}
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge {
			r.incDropped(int64(len(pending)))
			return SendRejected, serr
		}
		return SendRetry, serr
	}
	return SendOK, nil
}

var (
	errEncode       = errors.New("failed to encode events")
	errBuildRequest = errors.New("failed to build ingest request")
)

// statusError describes a non-success response by status code only.
func statusError(code int) error {
	return fmt.Errorf("ingest request failed: HTTP %d", code)
}

// transportError classifies a client.Do failure without exposing the wrapped
// *url.Error (which carries the destination URL).
func transportError(ctx context.Context) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errors.New("ingest request timed out")
	case errors.Is(ctx.Err(), context.Canceled):
		return errors.New("ingest request canceled")
	default:
		return errors.New("ingest request failed: network error")
	}
}

// validateIngestURL enforces the telemetry destination policy: an absolute
// https URL with a host and no userinfo, query, or fragment. Cleartext http is
// permitted only for numeric loopback addresses (local workflows/tests).
func validateIngestURL(raw string) (string, error) {
	invalid := errors.New("ingest URL rejected: must be absolute https with a host, no credentials, query, or fragment (http only for numeric loopback)")
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return "", invalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Host == "" {
		return "", invalid
	}
	host := u.Hostname()
	if host == "" {
		return "", invalid
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", invalid
		}
	default:
		return "", invalid
	}
	return u.String(), nil
}

// noRedirectClient returns a copy of c that never follows redirects, so the
// bearer token and event data cannot be forwarded to another destination.
// The caller's client is not mutated.
func noRedirectClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{
			Timeout:       DefaultTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

func (r *Reporter) setError(msg string) {
	r.mu.Lock()
	r.lastError = msg
	r.mu.Unlock()
}

func (r *Reporter) incDropped(n int64) {
	r.mu.Lock()
	r.dropped += n
	r.mu.Unlock()
}

func (r *Reporter) backoff() {
	r.mu.Lock()
	r.failures++
	shift := min(r.failures-1, 10)
	delay := BaseBackoff * time.Duration(1<<shift)
	if delay > MaxBackoff {
		delay = MaxBackoff
	}
	r.nextFlushAt = r.now().Add(delay)
	r.mu.Unlock()
}

func (r *Reporter) resetBackoff() {
	r.mu.Lock()
	r.failures = 0
	r.nextFlushAt = time.Time{}
	r.mu.Unlock()
}

func sanitizeEvent(ev UsageEvent) UsageEvent {
	ev.Kind = "usage"
	if ev.EventID == "" {
		ev.EventID = NewUUIDv4()
	}
	ev.SessionID = clampStr(ev.SessionID, 128, "tmproxy")
	ev.Provider = clampStr(ev.Provider, 64, "unknown")
	ev.Model = clampStr(ev.Model, 128, "unknown")
	ev.ClientVersion = clampStr(ev.ClientVersion, 32, "")
	ev.InputTokens = clampTokens(ev.InputTokens)
	ev.OutputTokens = clampTokens(ev.OutputTokens)
	ev.CacheReadTokens = clampTokens(ev.CacheReadTokens)
	ev.CacheWriteTokens = clampTokens(ev.CacheWriteTokens)
	if math.IsNaN(ev.CostUSD) || math.IsInf(ev.CostUSD, 0) || ev.CostUSD < 0 {
		ev.CostUSD = 0
	} else {
		ev.CostUSD = math.Round(ev.CostUSD*1e6) / 1e6
		if ev.CostUSD > 10_000 {
			ev.CostUSD = 10_000
		}
	}
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return ev
}

func clampStr(s string, maxLen int, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

func clampTokens(n int64) int64 {
	if n < 0 {
		return 0
	}
	if n > 1_000_000_000 {
		return 1_000_000_000
	}
	return n
}

// NewUUIDv4 generates a random RFC 4122 v4 UUID string.
func NewUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

// NewUUIDv5 derives a deterministic RFC 4122 v5 (SHA-1) UUID string for name.
func NewUUIDv5(name string) string {
	h := sha1.New()
	_, _ = h.Write(eventNamespace[:])
	_, _ = h.Write([]byte(name))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}

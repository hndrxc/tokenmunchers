package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const backendTime = "2006-01-02T15:04:05.000Z"

var (
	errEndpoint   = errors.New("desktop accounting requires a valid HTTPS or loopback HTTP /ingest URL")
	errDelivery   = errors.New("desktop contribution delivery failed")
	errConflict   = errors.New("desktop contribution conflict; pending snapshot retained")
	publisherHTTP = &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
)

func ValidateEndpoint(ingestURL string) error {
	_, err := contributionEndpoint(ingestURL)
	return err
}

func contributionEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || u.RawPath != "" || strings.ContainsAny(raw, "%?#\\") || !strings.HasSuffix(u.Path, "/ingest") {
		return "", errEndpoint
	}
	for _, segment := range strings.Split(u.Path[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errEndpoint
		}
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return "", errEndpoint
	}
	if ip == nil && (strings.ContainsAny(host, "[]:") || strings.ContainsAny(u.Host, "[]")) {
		return "", errEndpoint
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errEndpoint
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", errEndpoint
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/ingest") + "/contributions"
	return u.String(), nil
}

type snapshot struct {
	ID     string             `json:"id"`
	IDs    []string           `json:"ids"`
	Bodies [3]json.RawMessage `json:"bodies"`
}

type publication struct {
	Action   string `json:"action"`
	Device   string `json:"device_id"`
	Harness  string `json:"harness"`
	Snapshot string `json:"snapshot_id"`
}

type wireObservation struct {
	Harness          string   `json:"harness"`
	CanonicalID      string   `json:"canonical_id"`
	IdentityStrength string   `json:"identity_strength"`
	Grain            string   `json:"grain"`
	Timestamp        string   `json:"timestamp"`
	IntervalStart    *string  `json:"interval_start"`
	IntervalEnd      *string  `json:"interval_end"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	SessionID        *string  `json:"session_id"`
	ParentSessionID  *string  `json:"parent_session_id"`
	IsSubagent       *bool    `json:"is_subagent"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	CallCount        int      `json:"call_count"`
	CostUSD          *float64 `json:"cost_usd"`
	CostBasis        string   `json:"cost_basis"`
	Quality          string   `json:"quality"`
}

func makeSnapshot(s *sourceLedger, harness, id string, n int) (*snapshot, error) {
	p := &snapshot{ID: id, IDs: make([]string, n)}
	rows := make([]wireObservation, n)
	for i, o := range s.queue[:n] {
		p.IDs[i] = o.CanonicalID
		rows[i] = wireObservation{
			Harness: harness, CanonicalID: o.CanonicalID, IdentityStrength: o.IdentityStrength,
			Grain: "request", Timestamp: o.Timestamp.UTC().Format(backendTime), Provider: o.Provider, Model: o.Model,
			InputTokens: o.InputTokens, OutputTokens: o.OutputTokens, CacheReadTokens: o.CacheReadTokens, CacheWriteTokens: o.CacheWriteTokens,
			CallCount: 1, CostBasis: "unknown", Quality: "observed",
		}
	}
	common := publication{Device: s.Device, Harness: harness, Snapshot: id}
	common.Action = "begin"
	begin := struct {
		publication
		Cutoff     string `json:"cutoff"`
		ChunkCount int    `json:"chunk_count"`
	}{publication: common, Cutoff: s.Cutoff.UTC().Format(backendTime), ChunkCount: 1}
	common.Action = "chunk"
	chunk := struct {
		publication
		Index        int               `json:"index"`
		Observations []wireObservation `json:"observations"`
	}{publication: common, Index: 0, Observations: rows}
	common.Action = "commit"
	for i, msg := range [...]any{begin, chunk, common} {
		data, err := json.Marshal(msg)
		if err != nil || len(data) > 256*1024 {
			return nil, errCorrupt
		}
		p.Bodies[i] = data
	}
	return p, nil
}

// Validate persisted bodies against the v1 encoding before sending anything.
func (p *snapshot) valid(s *sourceLedger, harness string) bool {
	want, err := makeSnapshot(s, harness, p.ID, len(p.IDs))
	if err != nil {
		return false
	}
	for i, body := range p.Bodies {
		if !bytes.Equal(body, want.Bodies[i]) {
			return false
		}
	}
	return true
}

// Run polls the durable queue until cancellation. It reports only fixed messages
// and status codes; responses and transport errors can contain sensitive data.
func (c *Collector) Run(ctx context.Context, report func(error)) {
	const interval = 5 * time.Second
	blocked := make(map[string]bool, 2)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		delay := interval
		for _, harness := range [...]string{Claude, Codex} {
			if blocked[harness] || ctx.Err() != nil {
				continue
			}
			retry, err := c.publish(ctx, harness)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errConflict) {
				blocked[harness] = true
			}
			if err != nil && report != nil {
				report(err)
			}
			if retry > 0 {
				if retry > delay {
					delay = retry
				}
				break
			}
		}
		timer.Reset(delay)
	}
}

func (c *Collector) prepare(harness string) (*snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil
	}
	if c.journal.failed {
		return nil, errLedger
	}
	s := c.sources[harness]
	if s == nil || s.minimum.IsZero() || len(s.queue) == 0 {
		return nil, nil
	}
	if s.pending == nil {
		id, err := uuid()
		if err != nil {
			return nil, err
		}
		p, err := makeSnapshot(s, harness, id, min(len(s.queue), maxBatch))
		if err != nil {
			return nil, err
		}
		if err := c.journal.append(ledgerEntry{Kind: "snapshot", Harness: harness, Snapshot: p}); err != nil {
			return nil, err
		}
		s.pending = p
	}
	return s.pending, nil
}

func (c *Collector) publish(ctx context.Context, harness string) (time.Duration, error) {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	if ctx.Err() != nil {
		return 0, errDelivery
	}
	p, err := c.prepare(harness)
	if err != nil || p == nil {
		return 0, err
	}
	for i, action := range [...]string{"begin", "chunk", "commit"} {
		if retry, err := c.deliver(ctx, p, action, p.Bodies[i]); err != nil {
			return retry, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.journal.append(ledgerEntry{Kind: "ack", Harness: harness, Ack: p.ID}); err != nil {
		return 0, err
	}
	c.sources[harness].ack()
	return 0, nil
}

func (c *Collector) deliver(ctx context.Context, p *snapshot, action string, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, errDelivery
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	res, err := publisherHTTP.Do(req)
	if err != nil {
		return 0, errDelivery
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		retry := retryAfter(res.Header.Get("Retry-After"), time.Now())
		if res.StatusCode == http.StatusConflict {
			return retry, errConflict
		}
		if res.StatusCode == http.StatusTooManyRequests && retry == 0 {
			retry = 5 * time.Second
		}
		return retry, fmt.Errorf("desktop contribution delivery returned HTTP %d", res.StatusCode)
	}
	var ack struct {
		OK        bool   `json:"ok"`
		Action    string `json:"action"`
		Snapshot  string `json:"snapshot_id"`
		Committed bool   `json:"committed"`
		Accepted  int    `json:"accepted"`
	}
	const limit = 4096
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(data) > limit || json.Unmarshal(data, &ack) != nil || !ack.OK || ack.Action != action || ack.Snapshot != p.ID ||
		(action == "commit" && (!ack.Committed || ack.Accepted != len(p.IDs))) {
		return 0, errDelivery
	}
	return 0, nil
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		const maxSeconds = int64((1<<63 - 1) / time.Second)
		return time.Duration(min(seconds, maxSeconds)) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return deadline.Sub(now)
	}
	return 0
}

package desktop

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"
)

const (
	Claude   = "claude-code"
	Codex    = "codex"
	maxBatch = 100
)

type Observation struct {
	Harness, CanonicalID, IdentityStrength, Provider, Model      string
	Timestamp                                                    time.Time
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64
}

type Options struct {
	Dir, IngestURL, APIKey  string
	ClaudeSince, CodexSince time.Time
	Paused                  bool
}

var (
	ErrRejectedObservation = errors.New("desktop observation rejected: invalid metadata")
	errPaused              = errors.New("desktop accounting is paused; retry eligible pre-pause observation after resume")
	errLedger              = errors.New("desktop ledger storage failure; restart to recover")
	errCorrupt             = errors.New("desktop ledger contains an invalid record")
	errBinding             = errors.New("desktop ledger is bound to a different destination or key")
	claudeID               = regexp.MustCompile(`^req_[A-Za-z0-9_/-]+$`)
	codexID                = regexp.MustCompile(`^resp_[A-Za-z0-9_/-]+$`)
	modelName              = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Collector struct {
	mu               sync.Mutex
	publishMu        sync.Mutex
	journal, pauses  *journal
	stateLock        *os.File
	closed           bool
	binding          string
	endpoint, apiKey string
	sources          map[string]*sourceLedger
	since            map[string]time.Time // current activation per harness; zero when off
	windows          []pauseWindow
	pauseResumeFloor time.Time
}

type sourceLedger struct {
	Device    string
	Cutoff    time.Time
	minimum   time.Time
	seen      map[string]struct{}
	queue     []Observation
	pending   *snapshot
	delivered int // acknowledged calls timestamped at or after minimum
}

type pauseWindow struct{ start, end time.Time }

type ledgerEntry struct {
	Kind        string       `json:"kind"`
	Binding     string       `json:"binding,omitempty"`
	Harness     string       `json:"harness,omitempty"`
	Device      string       `json:"device,omitempty"`
	Cutoff      *time.Time   `json:"cutoff,omitempty"`
	Observation *Observation `json:"observation,omitempty"`
	Snapshot    *snapshot    `json:"snapshot,omitempty"`
	Ack         string       `json:"ack,omitempty"`
	At          *time.Time   `json:"at,omitempty"`
}

type journal struct {
	file   *os.File
	failed bool
}

func Open(opts Options) (*Collector, error) {
	endpoint, err := contributionEndpoint(opts.IngestURL)
	if err != nil {
		return nil, err
	}
	if opts.Dir == "" || opts.APIKey == "" {
		return nil, errors.New("desktop ledger requires a directory and API key")
	}
	activation := [2]time.Time{opts.ClaudeSince.UTC(), opts.CodexSince.UTC()}
	for _, since := range activation {
		if !since.IsZero() && !validTime(since) {
			return nil, errors.New("desktop ledger has an invalid activation time")
		}
	}
	if err := os.MkdirAll(opts.Dir, 0700); err != nil {
		return nil, errLedger
	}
	if err := privateDir(opts.Dir); err != nil {
		return nil, err
	}
	binding := fmt.Sprintf("%x", sha256.Sum256([]byte(endpoint+"\x00"+opts.APIKey)))
	c := &Collector{endpoint: endpoint, apiKey: opts.APIKey, sources: make(map[string]*sourceLedger),
		since: map[string]time.Time{Claude: activation[0], Codex: activation[1]}}
	c.stateLock, err = acquireStateLock(opts.Dir)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.Close()
		}
	}()
	c.journal, err = openJournal(opts.Dir, "ledger.jsonl", c.replay)
	if err != nil {
		return nil, err
	}
	if c.binding != "" && c.binding != binding {
		return nil, errBinding
	}
	if c.binding == "" {
		if err = c.journal.append(ledgerEntry{Kind: "binding-v1", Binding: binding}); err != nil {
			return nil, err
		}
		c.binding = binding
	}
	for i, harness := range [2]string{Claude, Codex} {
		since := activation[i]
		if since.IsZero() {
			continue
		}
		if c.sources[harness] == nil {
			device, err := uuid()
			if err != nil {
				return nil, err
			}
			cutoff := since.Truncate(time.Millisecond)
			entry := ledgerEntry{Kind: "source", Harness: harness, Device: device, Cutoff: &cutoff}
			if err := c.journal.append(entry); err != nil {
				return nil, err
			}
			if err := c.replay(entry); err != nil {
				return nil, err
			}
		}
	}
	c.pauses, err = openJournal(opts.Dir, "pause.jsonl", c.replayPause)
	if err != nil {
		return nil, err
	}
	// Config can start a pause, but never silently end a durable pause on startup.
	if opts.Paused {
		start := time.Now()
		if len(c.windows) == 0 {
			// A paused configuration with no prior transition does not tell
			// us when pausing began. Exclude delayed events since opt-in.
			for _, since := range activation {
				if !since.IsZero() && since.Before(start) {
					start = since
				}
			}
		}
		if err = c.setPaused(true, start); err != nil {
			return nil, err
		}
	}
	ok = true
	return c, nil
}

func (c *Collector) Close() error {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var err error
	for _, j := range []*journal{c.journal, c.pauses} {
		if j != nil && j.file.Close() != nil {
			err = errLedger
		}
	}
	if c.stateLock != nil && c.stateLock.Close() != nil {
		err = errLedger
	}
	return err
}

func (c *Collector) replay(entry ledgerEntry) error {
	if c.binding == "" {
		if entry.Kind != "binding-v1" || len(entry.Binding) != 64 {
			return errCorrupt
		}
		c.binding = entry.Binding
		return nil
	}
	s := c.sources[entry.Harness]
	switch entry.Kind {
	case "source":
		if s != nil || (entry.Harness != Claude && entry.Harness != Codex) || entry.Cutoff == nil || !uuidShape.MatchString(entry.Device) {
			return errCorrupt
		}
		c.sources[entry.Harness] = &sourceLedger{Device: entry.Device, Cutoff: *entry.Cutoff, minimum: c.since[entry.Harness], seen: make(map[string]struct{})}
	case "record":
		if s == nil || entry.Observation == nil || entry.Observation.Harness != entry.Harness || !validObservation(*entry.Observation) {
			return errCorrupt
		}
		if _, ok := s.seen[entry.Observation.CanonicalID]; ok {
			return errCorrupt
		}
		s.add(*entry.Observation)
	case "snapshot":
		p := entry.Snapshot
		if s == nil || s.pending != nil || p == nil || !uuidShape.MatchString(p.ID) || len(p.IDs) == 0 || len(p.IDs) > maxBatch || len(p.IDs) > len(s.queue) {
			return errCorrupt
		}
		for i, id := range p.IDs {
			if id != s.queue[i].CanonicalID {
				return errCorrupt
			}
		}
		if !p.valid(s, entry.Harness) {
			return errCorrupt
		}
		s.pending = p
	case "ack":
		if s == nil || s.pending == nil || s.pending.ID != entry.Ack {
			return errCorrupt
		}
		s.ack()
	default:
		return errCorrupt
	}
	return nil
}

// RecordedIDs reads a harness's collector ledger without taking its writer lock
// or repairing it. Both pending and acknowledged observations remain owned.
// Missing state is empty and invalid entries fail closed. An incomplete last
// line is ignored, as the collector discards it on recovery: it may be an
// append in progress, and a torn record was never queued for delivery.
func RecordedIDs(dir, harness string) (map[string]struct{}, error) {
	if harness != Claude && harness != Codex {
		return nil, errLedger
	}
	path := filepath.Join(dir, "ledger.jsonl")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errLedger
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errLedger
	}
	defer f.Close()
	c := &Collector{sources: make(map[string]*sourceLedger)}
	// Read a fixed prefix even if the daemon keeps appending.
	_, _, err = replayJournal(io.NewSectionReader(f, 0, info.Size()), c.replay)
	if err != nil {
		return nil, err
	}
	if s := c.sources[harness]; s != nil {
		return s.seen, nil
	}
	return nil, nil
}

func (s *sourceLedger) add(o Observation) {
	s.seen[o.CanonicalID] = struct{}{}
	s.queue = append(s.queue, o)
}

func (s *sourceLedger) ack() {
	n := len(s.pending.IDs)
	// Record compares untruncated provider times with minimum, then stores them
	// truncated to the millisecond.
	floor := s.minimum.Truncate(time.Millisecond)
	for _, o := range s.queue[:n] {
		if !o.Timestamp.Before(floor) {
			s.delivered++
		}
	}
	clear(s.queue[:n])
	s.queue = s.queue[n:]
	if len(s.queue) == 0 {
		s.queue = nil
	}
	s.pending = nil
}

func (c *Collector) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sources {
		n += len(s.queue)
	}
	return n
}

// Delivered counts calls TokenMunchers acknowledged since each enabled
// source's current activation, including deliveries before this process started.
func (c *Collector) Delivered() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sources {
		if !s.minimum.IsZero() {
			n += s.delivered
		}
	}
	return n
}

func (c *Collector) Record(o Observation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.journal.failed || c.pauses.failed {
		return errLedger
	}
	s := c.sources[o.Harness]
	if s == nil || s.minimum.IsZero() {
		return nil
	}
	o.Timestamp = o.Timestamp.UTC()
	if o.Timestamp.Before(s.minimum) || o.Timestamp.Before(s.Cutoff) {
		return nil
	}
	// Match provider timestamps at the precision published to the backend. A
	// partial millisecond at either end of a pause is excluded conservatively.
	o.Timestamp = o.Timestamp.Truncate(time.Millisecond)
	i := sort.Search(len(c.windows), func(i int) bool { return c.windows[i].end.IsZero() || c.windows[i].end.After(o.Timestamp) })
	if i < len(c.windows) && !o.Timestamp.Before(c.windows[i].start) {
		return nil
	}
	if o.Model == "" {
		o.Model = "unknown"
	}
	if !validObservation(o) || !validTime(o.Timestamp) {
		return ErrRejectedObservation
	}
	if _, ok := s.seen[o.CanonicalID]; ok {
		return nil
	}
	// Do not consume delayed pre-pause events while paused. Claude may retry;
	// Codex can retry the same source line without needing an app restart.
	if c.paused() {
		return errPaused
	}
	if err := c.journal.append(ledgerEntry{Kind: "record", Harness: o.Harness, Observation: &o}); err != nil {
		return err
	}
	s.add(o)
	return nil
}

func validTime(t time.Time) bool {
	return t.UnixMilli() >= 0 && !t.After(time.Now().Add(5*time.Minute))
}

func validObservation(o Observation) bool {
	if len(o.CanonicalID) > 128 || !modelName.MatchString(o.Model) {
		return false
	}
	switch o.Harness {
	case Claude:
		if o.Provider != "anthropic" || o.IdentityStrength != "request" || !claudeID.MatchString(o.CanonicalID) {
			return false
		}
	case Codex:
		if o.Provider != "openai" || o.IdentityStrength != "response" || !codexID.MatchString(o.CanonicalID) {
			return false
		}
	default:
		return false
	}
	for _, n := range [...]int64{o.InputTokens, o.OutputTokens, o.CacheReadTokens, o.CacheWriteTokens} {
		if n < 0 || n > 1_000_000_000 {
			return false
		}
	}
	return true
}

func (c *Collector) paused() bool {
	return len(c.windows) != 0 && c.windows[len(c.windows)-1].end.IsZero()
}

func (c *Collector) replayPause(e ledgerEntry) error {
	if e.At == nil || e.At.IsZero() {
		return errCorrupt
	}
	at := *e.At
	switch e.Kind {
	case "pause":
		if c.paused() {
			return errCorrupt
		}
		i := sort.Search(len(c.windows), func(i int) bool { return !c.windows[i].end.Before(at) })
		// Merge adjacent windows, also covering a backwards wall clock.
		if n := len(c.windows); i < n {
			c.pauseResumeFloor = c.windows[n-1].end
		}
		if i < len(c.windows) && c.windows[i].start.Before(at) {
			at = c.windows[i].start
		}
		c.windows = append(c.windows[:i], pauseWindow{start: at})
	case "resume":
		if !c.paused() {
			return errCorrupt
		}
		w := &c.windows[len(c.windows)-1]
		if at.Before(w.start) {
			return errCorrupt
		}
		w.end = at
		c.pauseResumeFloor = time.Time{}
	default:
		return errCorrupt
	}
	return nil
}

func (c *Collector) SetPaused(paused bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setPaused(paused, time.Now())
}

// PersistPause is for stopped Munch, which does not need a working publishing key.
func PersistPause(dir string, paused bool) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errLedger
	}
	if err := privateDir(dir); err != nil {
		return err
	}
	c := &Collector{}
	lock, err := acquireStateLock(dir)
	if err != nil {
		return err
	}
	c.stateLock = lock
	j, err := openJournal(dir, "pause.jsonl", c.replayPause)
	if err != nil {
		return errors.Join(err, c.Close())
	}
	c.pauses = j
	return errors.Join(c.setPaused(paused, time.Now()), c.Close())
}

func (c *Collector) setPaused(paused bool, now time.Time) error {
	if c.closed || c.pauses.failed {
		return errLedger
	}
	if c.paused() == paused {
		return nil
	}
	at := now.UTC().Truncate(time.Millisecond)
	kind := "pause"
	if !paused {
		kind = "resume"
		at = at.Add(time.Millisecond)
		if start := c.windows[len(c.windows)-1].start; at.Before(start) {
			at = start
		}
		if at.Before(c.pauseResumeFloor) {
			at = c.pauseResumeFloor
		}
	}
	e := ledgerEntry{Kind: kind, At: &at}
	if err := c.pauses.append(e); err != nil {
		return err
	}
	return c.replayPause(e)
}

func privateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || os.Chmod(dir, 0700) != nil {
		return errLedger
	}
	return nil
}

func openJournal(dir, name string, apply func(ledgerEntry) error) (*journal, error) {
	path := filepath.Join(dir, name)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errLedger
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errLedger
	}
	// The state lock guarantees a single writer. Avoid O_APPEND so Windows
	// opens with the write access required to truncate a torn tail.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, errLedger
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	if f.Chmod(0600) != nil {
		return nil, errLedger
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err != nil {
			return nil, errLedger
		}
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return nil, errLedger
		}
	}
	offset, torn, err := replayJournal(f, apply)
	if err != nil {
		return nil, err
	}
	if torn && (f.Truncate(offset) != nil || f.Sync() != nil) {
		return nil, errLedger
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return nil, errLedger
	}
	ok = true
	return &journal{file: f}, nil
}

func replayJournal(src io.Reader, apply func(ledgerEntry) error) (offset int64, torn bool, err error) {
	r := bufio.NewReaderSize(src, 512*1024)
	for {
		line, err := r.ReadSlice('\n')
		if err == io.EOF {
			return offset, len(line) != 0, nil
		}
		if err != nil {
			return offset, false, errLedger
		}
		var e ledgerEntry
		if json.Unmarshal(line, &e) != nil {
			return offset, false, errCorrupt
		}
		if err := apply(e); err != nil {
			return offset, false, err
		}
		offset += int64(len(line))
	}
}

func (j *journal) append(e ledgerEntry) error {
	if j.failed {
		return errLedger
	}
	data, err := json.Marshal(e)
	if err != nil {
		return errLedger
	}
	data = append(data, '\n')
	if n, err := j.file.Write(data); err != nil || n != len(data) || j.file.Sync() != nil {
		j.failed = true
		return errLedger
	}
	return nil
}

var uuidShape = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func uuid() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("desktop ledger could not allocate identity")
	}
	b[6] = (b[6] & 0xf) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

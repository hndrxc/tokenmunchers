package backfill

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const ExclUnverifiedOutput = "unverified_output"

// Desktop timestamps its live event at completion, after the transcript
// frame may already have been saved. Reserve that small boundary overlap.
const claudeDesktopClockGuard = time.Second

// ScanClaude returns only exactly reconciled local Claude Code responses. With
// no window, these are observed preview candidates, not a verified upload set.
func ScanClaude(ctx context.Context, root string, window Window, desktop DesktopOwnership) (Preview, error) {
	s := claudeScan{
		excluded: make(map[string]int), desktop: desktop,
		messages: make(map[string]*claudeCall), requests: make(map[string]*claudeCall),
	}
	if err := ctx.Err(); err != nil {
		return Preview{}, err
	}
	if root == "" {
		return Preview{Excluded: s.excluded}, nil
	}
	if err := s.scan(ctx, root); err != nil {
		return Preview{}, err
	}
	for _, g := range s.groups {
		if err := ctx.Err(); err != nil {
			return Preview{}, err
		}
		g.verify()
	}
	out := Preview{Excluded: s.excluded}
	for _, c := range s.messages {
		if err := ctx.Err(); err != nil {
			return Preview{}, err
		}
		var skip string
		switch {
		case c.conflict:
			skip = ExclConflict
		case c.ID == "":
			skip = ExclMissingIdentity
		case c.owned:
			skip = ExclDesktopCollected
		case !c.verified:
			skip = ExclUnverifiedOutput
		case !c.linked:
			skip = ExclMissingIdentity
		case c.counts == (claudeCounts{}):
			skip = ExclAllZero
		case c.attribution.time.IsZero():
			skip = ExclMissingTimestamp
		case !window.IsZero() && (c.attribution.time.Before(window.From) || !c.attribution.time.Before(window.Before)):
			skip = ExclOutsideWindow
		default:
			c.Record.Timestamp = c.attribution.time
			c.Record.Subagent = &c.attribution.subagent
			c.Record.Provider = c.attribution.provider
			if c.Record.Provider == "" || c.attribution.providerConflict {
				c.Record.Provider = unknownProvider
			}
			c.Input, c.Output, c.CacheRead, c.CacheWrite = c.counts[0], c.counts[1], c.counts[2], c.counts[3]
			out.Records = append(out.Records, c.Record)
		}
		if skip != "" {
			s.excluded[skip]++
		}
	}
	sort.Slice(out.Records, func(i, j int) bool {
		a, b := out.Records[i], out.Records[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		return a.ID < b.ID
	})
	return out, nil
}

type claudeScan struct {
	excluded map[string]int
	desktop  DesktopOwnership
	messages map[string]*claudeCall
	// A present nil value reserves an ambiguous or malformed request ID.
	requests map[string]*claudeCall
	groups   []*claudeGroup
}

type claudeCall struct {
	Record
	counts                                           claudeCounts
	attribution                                      claudeAttribution
	seen, invalid, conflict, verified, owned, linked bool
}

type claudeGroup struct {
	members       map[*claudeCall]claudeMember
	invalidModels map[string]bool
	state         map[string]claudeTotals
	stateLine     int
	stateTime     time.Time
	incomplete    bool
}

type claudeMember struct {
	firstMainLine int
	linked        bool
	claudeAttribution
}

type claudeAttribution struct {
	time                       time.Time
	provider                   string
	subagent, providerConflict bool
}

func (a *claudeAttribution) merge(b claudeAttribution) {
	if b.provider != "" {
		if a.provider != "" && a.provider != b.provider {
			a.providerConflict = true
		} else {
			a.provider = b.provider
		}
	}
	a.providerConflict = a.providerConflict || b.providerConflict
	if !b.time.IsZero() {
		if a.time.IsZero() || b.time.Before(a.time) {
			a.time, a.subagent = b.time, b.subagent
		} else if b.time.Equal(a.time) {
			a.subagent = a.subagent || b.subagent
		}
	}
}

// Each group must independently account for every distinct message in its
// files, even if another group has a copy, or it cannot publish the message.
func (g *claudeGroup) verify() {
	if g.incomplete {
		return
	}
	// Incomplete files can disclose overlap or ambiguity, but cannot supply
	// the publishable request pairing, timestamp, or attribution.
	for c, m := range g.members {
		c.linked = c.linked || m.linked
		c.attribution.merge(m.claudeAttribution)
	}
	if g.state == nil {
		return
	}
	models := make(map[string]claudeCounts)
	for c, m := range g.members {
		if c.conflict || c.invalid || !c.seen || m.firstMainLine > g.stateLine ||
			(!g.stateTime.IsZero() && !m.time.IsZero() && m.time.After(g.stateTime)) {
			g.invalidate(c.Model)
			continue
		}
		sum := models[c.Model]
		for i, n := range c.counts {
			sum[i] += n
		}
		models[c.Model] = sum
	}
	if g.incomplete {
		return
	}
	for model, sum := range models {
		want, ok := g.state[model]
		counts, valid := want.counts()
		if g.invalidModels[model] || !ok || !valid || counts != sum {
			delete(models, model)
		}
	}
	for c := range g.members {
		if _, ok := models[c.Model]; ok {
			c.verified = true
		}
	}
}

func (g *claudeGroup) invalidate(model string) {
	if model == "" {
		g.incomplete = true
		return
	}
	if g.invalidModels == nil {
		g.invalidModels = make(map[string]bool)
	}
	g.invalidModels[model] = true
}

// Reject link components before opening, and scope subsequent opens to the
// chosen root so a concurrently replaced entry cannot escape the config home.
func (s *claudeScan) scan(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("backfill: invalid Claude config root")
	}
	var ancestors []string
	for p := abs; ; p = filepath.Dir(p) {
		ancestors = append(ancestors, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		info, err := os.Lstat(ancestors[i])
		if err != nil || !info.IsDir() || !claudePlain(info.Mode()) {
			return fmt.Errorf("backfill: Claude config root is unavailable or contains a link")
		}
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("backfill: unable to open Claude config root")
	}
	defer root.Close()
	projects, err := claudeReadDir(root, "projects")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backfill: unable to read Claude projects directory")
	}
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !project.IsDir() || !claudePlain(project.Type()) {
			continue
		}
		base := filepath.Join("projects", project.Name())
		entries, err := claudeReadDir(root, base)
		if err != nil {
			s.excluded[ExclReadFailed]++
			continue
		}
		groups := make(map[string]*claudeGroup)
		group := func(id string) *claudeGroup {
			g := groups[id]
			if g == nil {
				g = &claudeGroup{members: make(map[*claudeCall]claudeMember)}
				groups[id] = g
				s.groups = append(s.groups, g)
			}
			return g
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			if entry.Type().IsRegular() && strings.HasSuffix(name, ".jsonl") && len(name) > len(".jsonl") {
				id := strings.TrimSuffix(name, ".jsonl")
				s.read(ctx, root, filepath.Join(base, name), id, false, group(id))
			}
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			id := entry.Name()
			if !claudePlain(entry.Type()) {
				if g := groups[id]; g != nil {
					g.incomplete = true
					s.excluded[ExclReadFailed]++
				}
				continue
			}
			if !entry.IsDir() {
				continue
			}
			sessionDir := filepath.Join(base, id)
			info, err := root.Lstat(sessionDir)
			if err != nil || !info.IsDir() || !claudePlain(info.Mode()) {
				group(id).incomplete = true
				s.excluded[ExclReadFailed]++
				continue
			}
			sub := filepath.Join(sessionDir, "subagents")
			agents, err := claudeReadDir(root, sub)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				group(id).incomplete = true
				s.excluded[ExclReadFailed]++
				continue
			}
			for _, agent := range agents {
				name := agent.Name()
				if strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl") {
					s.read(ctx, root, filepath.Join(sub, name), id, true, group(id))
				}
			}
		}
	}
	return ctx.Err()
}

func claudePlain(mode fs.FileMode) bool {
	return mode&(fs.ModeSymlink|fs.ModeIrregular) == 0
}

func claudeReadDir(root *os.Root, path string) ([]os.DirEntry, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !claudePlain(info.Mode()) {
		return nil, fs.ErrInvalid
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fs.ErrInvalid
	}
	return f.ReadDir(-1)
}

func (s *claudeScan) read(ctx context.Context, root *os.Root, path, session string, subagent bool, g *claudeGroup) {
	failure := func(reason string) {
		g.incomplete = true
		s.excluded[reason]++
	}
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		failure(ExclReadFailed)
		return
	}
	f, err := root.Open(path)
	if err != nil {
		failure(ExclReadFailed)
		return
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		failure(ExclReadFailed)
		return
	}
	lr := lineReader{r: bufio.NewReaderSize(f, 64<<10)}
	for line := 1; ; line++ {
		if ctx.Err() != nil {
			return
		}
		raw, status := lr.next(maxLineBytes)
		switch status {
		case lineEOF:
			// An active append during the read must not establish completeness.
			end, err := f.Stat()
			if err != nil || end.Size() != opened.Size() || !end.ModTime().Equal(opened.ModTime()) {
				failure(ExclReadFailed)
			}
			return
		case lineIncomplete:
			failure(ExclIncompleteLine)
			return
		case lineReadError:
			failure(ExclReadFailed)
			return
		case lineOversize:
			failure(ExclOversizedLine)
			continue
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry claudeEntry
		if err := json.Unmarshal(raw, &entry); err != nil || entry.Type == "" {
			failure(ExclMalformedJSON)
			// A type error in metadata can still leave recognizable IDs.
			if entry.Type == "assistant" {
				s.assistant(entry, subagent, line, g, false)
			}
			continue
		}
		switch entry.Type {
		case "assistant":
			s.assistant(entry, subagent, line, g, true)
		case "cost-state":
			if !subagent {
				// Never fall back to an older summary if the last row is unusable.
				g.state = nil
				g.stateLine = line
				g.stateTime = claudeTime(entry.Timestamp)
				if entry.Session == session && (entry.Timestamp == "" || !g.stateTime.IsZero()) {
					g.state = entry.ModelUsage
				}
			}
		}
	}
}

// Unknown JSON fields, including the transcript content and all free-form
// cost data, are skipped by encoding/json and never retained or displayed.
type claudeEntry struct {
	Type       string `json:"type"`
	Session    string `json:"sessionId"`
	Request    string `json:"requestId"`
	Timestamp  string `json:"timestamp"`
	Entrypoint string `json:"entrypoint"`
	Provider   string `json:"provider"`
	Sidechain  bool   `json:"isSidechain"`
	APIError   bool   `json:"isApiErrorMessage"`
	Message    struct {
		ID    string      `json:"id"`
		Model string      `json:"model"`
		Usage claudeUsage `json:"usage"`
	} `json:"message"`
	ModelUsage map[string]claudeTotals `json:"modelUsage"`
}

type claudeCounts [4]int64

type claudeUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
}

type claudeTotals struct {
	Input      *int64 `json:"inputTokens"`
	Output     *int64 `json:"outputTokens"`
	CacheRead  *int64 `json:"cacheReadInputTokens"`
	CacheWrite *int64 `json:"cacheCreationInputTokens"`
}

func (u claudeTotals) counts() (claudeCounts, bool) {
	return claudeTokenCounts([4]*int64{u.Input, u.Output, u.CacheRead, u.CacheWrite}, false)
}

func claudeTokenCounts(fields [4]*int64, bounded bool) (claudeCounts, bool) {
	var counts claudeCounts
	for i, p := range fields {
		if p == nil || *p < 0 || (bounded && *p > maxBucket) {
			return claudeCounts{}, false
		}
		counts[i] = *p
	}
	return counts, true
}

var claudeModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/@-]{0,127}$`)

func (s *claudeScan) assistant(e claudeEntry, subagent bool, line int, g *claudeGroup, decoded bool) {
	msg, req, model := e.Message.ID, e.Request, e.Message.Model
	u := e.Message.Usage
	// Explicit API error placeholders with neither usage nor identities do not
	// contribute; an error row carrying accounting evidence must reconcile.
	if decoded && e.APIError && msg == "" && req == "" && u == (claudeUsage{}) {
		return
	}
	if !claudeModel.MatchString(model) || model == unknownModel {
		model = ""
	}
	validReq := validClaudeWireID(req, "req_")
	if !validClaudeWireID(msg, "msg_") {
		if msg == "" {
			s.excluded[ExclMissingIdentity]++
		} else {
			s.excluded[ExclUnsafeID]++
		}
		if validReq {
			if c := s.requests[req]; c != nil {
				c.conflict = true
			}
			s.requests[req] = nil
		}
		g.invalidate(model)
		return
	}
	c := s.messages[msg]
	if c == nil {
		c = &claudeCall{Record: Record{ResponseID: msg}}
		s.messages[msg] = c
	} else {
		s.excluded[ExclDuplicate]++
	}
	if validReq {
		if other, ok := s.requests[req]; ok && other != c {
			c.conflict = true
			if other != nil {
				other.conflict = true
			}
			s.requests[req] = nil
		} else {
			s.requests[req] = c
		}
		if c.ID != "" && c.ID != req {
			c.conflict = true
		} else {
			c.ID = req
		}
	} else if req != "" {
		s.excluded[ExclUnsafeID]++
		c.conflict = true
	}
	if _, recorded := s.desktop.RecordedIDs[req]; recorded {
		c.owned = true
	}
	ts := claudeTime(e.Timestamp)
	// Use every frame's raw clock for the live reservation, not the chosen
	// earliest attribution (an earlier frame or fork can straddle activation).
	if !s.desktop.Since.IsZero() && e.Entrypoint != "cli" && e.Entrypoint != "sdk-cli" &&
		(ts.IsZero() || !ts.Before(s.desktop.Since.Add(-claudeDesktopClockGuard))) {
		c.owned = true
	}
	m := g.members[c]
	if !subagent && m.firstMainLine == 0 {
		m.firstMainLine = line
	}
	counts, valid := claudeTokenCounts([4]*int64{u.Input, u.Output, u.CacheRead, u.CacheWrite}, true)
	if model != "" {
		if c.Model != "" && c.Model != model {
			c.conflict = true
			g.invalidate(model)
		} else {
			c.Model = model
		}
	}
	if !decoded || !valid || model == "" || e.APIError {
		c.invalid = true
		g.invalidate(model)
		s.excluded[ExclMalformedUsage]++
		g.members[c] = m
		return
	}
	if c.seen && c.counts != counts {
		c.conflict = true
		g.invalidate(model)
		g.members[c] = m
		return
	}
	c.counts, c.seen = counts, true
	m.linked = m.linked || validReq
	m.merge(claudeAttribution{time: ts, subagent: subagent || e.Sidechain, provider: claudeProvider(e.Provider)})
	g.members[c] = m
}

func claudeTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.Year() < 1970 || t.Year() > 9999 {
		return time.Time{}
	}
	return t.UTC()
}

func claudeProvider(raw string) string {
	switch raw {
	case "anthropic", "firstParty", "first-party":
		return "anthropic"
	case "bedrock", "vertex", "foundry", "mantle", "gateway":
		return raw
	case "anthropicAws", "anthropic-aws":
		return "anthropic-aws"
	default:
		return ""
	}
}

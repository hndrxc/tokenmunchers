package desktop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const codexLineLimit = 128 * 1024

// CodexHome resolves the directory containing the local Codex rollout files.
func CodexHome() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot resolve Codex home")
	}
	return filepath.Join(home, ".codex"), nil
}

// ScanCodex follows live, native Codex Desktop responses until ctx is canceled.
func ScanCodex(ctx context.Context, home string, record func(Observation) error, report func(error)) {
	scanner := newCodexScanner(home, record, report)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		scanner.scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type codexScanner struct {
	home   string
	files  map[string]*codexFile
	record func(Observation) error
	report func(error)
	reader *bufio.Reader
}

type codexFile struct {
	info             fs.FileInfo
	offset           int64
	discarding       bool
	metadata, native bool
	models           map[string]string
}

func newCodexScanner(home string, record func(Observation) error, report func(error)) *codexScanner {
	return &codexScanner{
		home: home, files: make(map[string]*codexFile),
		record: record, report: report, reader: bufio.NewReaderSize(nil, codexLineLimit),
	}
}

var (
	errCodexWalk   = errors.New("cannot enumerate Codex rollouts")
	errCodexRead   = errors.New("cannot read a Codex rollout")
	errCodexRecord = errors.New("cannot save Codex Desktop usage")
)

func (s *codexScanner) warn(err error) {
	if s.report != nil {
		s.report(err)
	}
}

func (s *codexScanner) scan(ctx context.Context) {
	if ctx.Err() != nil || s.home == "" {
		return
	}
	info, err := os.Lstat(s.home)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.warn(errCodexWalk)
		}
		return
	}
	if info.Mode().Type() != fs.ModeDir {
		return
	}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(s.home, dir)
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					s.warn(errCodexWalk)
				}
				return nil
			}
			if entry.IsDir() && entry.Type() != fs.ModeDir {
				return fs.SkipDir
			}
			if path != root && entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "rollout-") && strings.HasSuffix(entry.Name(), ".jsonl") {
				s.scanFile(ctx, path, entry)
			}
			return nil
		})
		if ctx.Err() != nil {
			return
		}
	}
	for path := range s.files {
		if ctx.Err() != nil {
			return
		}
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			delete(s.files, path)
		}
	}
}

func (s *codexScanner) scanFile(ctx context.Context, path string, entry fs.DirEntry) {
	before, err := entry.Info()
	if err != nil || !before.Mode().IsRegular() {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.warn(errCodexRead)
		}
		return
	}
	state := s.files[path]
	if state != nil && before.Size() == state.offset && os.SameFile(state.info, before) {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.warn(errCodexRead)
		}
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) {
		s.warn(errCodexRead)
		return
	}
	if state == nil || !os.SameFile(state.info, info) || info.Size() < state.offset {
		state = &codexFile{info: info}
		s.files[path] = state
	}
	if _, err := file.Seek(state.offset, io.SeekStart); err != nil {
		s.warn(errCodexRead)
		return
	}
	s.reader.Reset(file)
	for ctx.Err() == nil {
		line, err := s.reader.ReadSlice('\n')
		switch err {
		case bufio.ErrBufferFull:
			// Keep skipping an oversized line across polls until its newline.
			state.discarding = true
		case io.EOF:
			if state.discarding {
				state.offset += int64(len(line))
			}
			return
		case nil:
			if !state.discarding && !s.consume(state, line) {
				return
			}
			state.discarding = false
		default:
			s.warn(errCodexRead)
			return
		}
		state.offset += int64(len(line))
	}
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexUsage struct {
	Input     *int64 `json:"input_tokens"`
	Output    *int64 `json:"output_tokens"`
	Read      *int64 `json:"cached_input_tokens"`
	Write     *int64 `json:"cache_write_input_tokens"`
	Reasoning *int64 `json:"reasoning_output_tokens"`
	Total     *int64 `json:"total_tokens"`
}

func (s *codexScanner) consume(state *codexFile, line []byte) bool {
	// Ignore unrelated envelopes without copying potentially large payloads.
	if !bytes.Contains(line, []byte("token_usage_record")) && !bytes.Contains(line, []byte("turn_context")) && !bytes.Contains(line, []byte("session_meta")) {
		return true
	}
	var row codexLine
	if json.Unmarshal(line, &row) != nil {
		return true
	}
	switch row.Type {
	case "session_meta":
		if !state.metadata {
			state.metadata = true
			var p struct {
				Originator string `json:"originator"`
				Provider   string `json:"model_provider"`
			}
			state.native = json.Unmarshal(row.Payload, &p) == nil && p.Originator == "Codex Desktop" && p.Provider == "openai"
		}
	case "turn_context":
		if !state.native {
			return true
		}
		var p struct {
			Turn  string `json:"turn_id"`
			Model string `json:"model"`
		}
		if json.Unmarshal(row.Payload, &p) == nil && codexSimple(p.Turn, false) {
			if codexSimple(p.Model, true) {
				if state.models == nil {
					state.models = make(map[string]string)
				}
				state.models[p.Turn] = p.Model
			} else {
				delete(state.models, p.Turn)
			}
		}
	case "token_usage_record":
		if !state.native {
			return true
		}
		var p struct {
			Turn     string     `json:"turn_id"`
			Response string     `json:"response_id"`
			Usage    codexUsage `json:"usage"`
		}
		if json.Unmarshal(row.Payload, &p) != nil ||
			!strings.HasPrefix(p.Response, "resp_") || len(p.Response) <= len("resp_") || !codexSimple(p.Response, false) || !p.Usage.valid() {
			return true
		}
		ts, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil || ts.Unix() < 0 {
			return true
		}
		model := state.models[p.Turn]
		if model == "" {
			model = "unknown"
		}
		if err := s.record(Observation{
			Harness: Codex, CanonicalID: p.Response, IdentityStrength: "response",
			Provider: "openai", Model: model, Timestamp: ts.UTC(),
			InputTokens: *p.Usage.Input - *p.Usage.Read - *p.Usage.Write, OutputTokens: *p.Usage.Output,
			CacheReadTokens: *p.Usage.Read, CacheWriteTokens: *p.Usage.Write,
		}); err != nil && !errors.Is(err, ErrRejectedObservation) {
			s.warn(errCodexRecord)
			return false
		}
	}
	return true
}

func (u codexUsage) valid() bool {
	for _, count := range [...]*int64{u.Input, u.Output, u.Read, u.Write} {
		if count == nil || *count < 0 || *count > 1_000_000_000 {
			return false
		}
	}
	return *u.Read+*u.Write <= *u.Input &&
		(u.Reasoning == nil || (*u.Reasoning >= 0 && *u.Reasoning <= *u.Output)) &&
		(u.Total == nil || *u.Total == *u.Input+*u.Output)
}

func codexSimple(value string, model bool) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		if i == 0 || (c != '_' && c != '-' && !(model && c == '.')) {
			return false
		}
	}
	return true
}

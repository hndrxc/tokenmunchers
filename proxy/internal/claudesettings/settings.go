// Package claudesettings edits Claude Desktop's user settings.json to enable
// the local OTLP telemetry stream Munch's desktop accounting pilot collects.
// It uses the same Inspect -> Plan -> Apply shape as internal/codex: settings
// Munch did not write are never overwritten, edits are verified by re-parsing,
// and files are replaced atomically. settings.json is Claude's own file, so
// every edit preserves the keys, their order, and the file's line endings.
package claudesettings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/proxy"
)

const (
	// maxSettingsSize is the largest settings.json Munch will read.
	maxSettingsSize = 1 << 20

	// loopbackHost matches the hosts Munch listens on, and localhost.
	loopbackHost = `(?:127(?:\.[0-9]{1,3}){3}|\[::1\]|localhost)`

	// logsEndpointKey is the env key holding the secret OTLP endpoint.
	logsEndpointKey = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
)

var (
	// endpointRe matches the exact Munch OTLP logs endpoint:
	// http://<loopback>:<port>/_tm/<64 hex>/__desktop/v1/logs.
	endpointRe = regexp.MustCompile(`^http://` + loopbackHost + `:[0-9]{1,5}/_tm/[0-9a-f]{64}` +
		regexp.QuoteMeta(proxy.ClaudeLogsPath) + `$`)
)

// envNames are the telemetry keys Munch manages, in the order they are
// appended to settings.json.
var envNames = []string{
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"OTEL_LOGS_EXPORTER",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_LOGS_EXPORT_INTERVAL",
	"OTEL_METRICS_EXPORTER",
	"OTEL_TRACES_EXPORTER",
	"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA",
	"OTEL_METRICS_INCLUDE_ENTRYPOINT",
	"OTEL_METRICS_INCLUDE_SESSION_ID",
	"OTEL_METRICS_INCLUDE_ACCOUNT_UUID",
	"OTEL_METRICS_INCLUDE_RESOURCE_ATTRIBUTES",
	"OTEL_RESOURCE_INCLUDE_HOST",
	"OTEL_LOG_USER_PROMPTS",
	"OTEL_LOG_ASSISTANT_RESPONSES",
	"OTEL_LOG_TOOL_DETAILS",
	"OTEL_LOG_TOOL_CONTENT",
	"OTEL_LOG_MANAGED_SETTINGS",
	"OTEL_LOG_RAW_API_BODIES",
}

// isTelemetryKey reports whether name configures Claude Code's OpenTelemetry
// export: any OTEL_* key or one of the keys Munch manages.
func isTelemetryKey(name string) bool {
	return strings.HasPrefix(name, "OTEL_") || slices.Contains(envNames, name)
}

// Dir returns the Claude user settings directory: $CLAUDE_CONFIG_DIR when it
// is set, else home/.claude.
func Dir(env func(string) string, home string) string {
	if dir := strings.TrimSpace(env("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}

// Path returns the path of Claude's user settings.json in dir.
func Path(dir string) string {
	return filepath.Join(dir, "settings.json")
}

// Env returns the telemetry environment Claude Desktop's local Code sessions
// export. endpoint is the secret OTLP logs endpoint Munch serves; it is the
// single source of the 19 keys Munch manages.
func Env(endpoint string) map[string]string {
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":             "1",
		"OTEL_LOGS_EXPORTER":                       "otlp",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":         "http/json",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":         endpoint,
		"OTEL_LOGS_EXPORT_INTERVAL":                "1000",
		"OTEL_METRICS_EXPORTER":                    "none",
		"OTEL_TRACES_EXPORTER":                     "none",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":      "0",
		"OTEL_METRICS_INCLUDE_ENTRYPOINT":          "true",
		"OTEL_METRICS_INCLUDE_SESSION_ID":          "false",
		"OTEL_METRICS_INCLUDE_ACCOUNT_UUID":        "false",
		"OTEL_METRICS_INCLUDE_RESOURCE_ATTRIBUTES": "false",
		"OTEL_RESOURCE_INCLUDE_HOST":               "0",
		"OTEL_LOG_USER_PROMPTS":                    "0",
		"OTEL_LOG_ASSISTANT_RESPONSES":             "0",
		"OTEL_LOG_TOOL_DETAILS":                    "0",
		"OTEL_LOG_TOOL_CONTENT":                    "0",
		"OTEL_LOG_MANAGED_SETTINGS":                "0",
		"OTEL_LOG_RAW_API_BODIES":                  "0",
	}
}

// ErrChanged means the settings file changed while Munch was preparing its
// edit. Nothing was written.
var ErrChanged = errors.New("the Claude settings file changed while munch was editing it; run the command again")

// Status says how a settings file relates to the proxy Munch is setting up.
type Status int

const (
	// Unconfigured: no telemetry settings are present. Apply adds the 19
	// Munch keys to env (or creates env if the file has none).
	Unconfigured Status = iota

	// Current: env holds exactly Munch's 19 settings for this endpoint.
	// Apply does nothing.
	Current

	// Stale: env holds exactly Munch's 19 settings for another Munch route.
	// Apply repoints only the endpoint.
	Stale

	// Blocked: the file holds something Munch must not overwrite, or cannot
	// edit without changing other settings. Plan.Reason says what.
	Blocked
)

// Plan is what Inspect found in a settings file and what Apply will do about
// it.
type Plan struct {
	// Path is the settings file as given to Inspect.
	Path string

	Status Status

	// Reason says why Status is Blocked. It never contains the endpoint or
	// any other value from the file.
	Reason string

	file    settingsFile
	updated string
}

// NeedsWrite reports whether Apply will change the file.
func (p *Plan) NeedsWrite() bool { return p.updated != "" }

// settingsFile is a settings.json as read from disk.
type settingsFile struct {
	// target is the file behind the path after following symlinks: the one
	// edits replace, so a symlinked settings stays a symlink.
	target  string
	content string

	// mode holds the permission bits of an existing file.
	mode   os.FileMode
	exists bool

	// unusable is set when the file is not a regular file or is too large.
	// content is then empty.
	unusable string
}

// readSettings reads the settings at path, following symlinks. A missing file
// reads as empty.
func readSettings(path string) (settingsFile, error) {
	target, err := resolve(path)
	if err != nil {
		return settingsFile{}, err
	}
	f := settingsFile{target: target}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return settingsFile{}, err
	}
	f.exists = true
	f.mode = info.Mode().Perm()
	switch {
	case !info.Mode().IsRegular():
		f.unusable = "it is not a regular file"
		return f, nil
	case info.Size() > maxSettingsSize:
		f.unusable = "it is larger than 1 MiB"
		return f, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return settingsFile{}, err
	}
	f.content = string(data)
	return f, nil
}

// resolve follows symlinks when path exists.
func resolve(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// object is a JSON object that keeps key order.
type object struct {
	keys []string
	m    map[string]any
}

func newObject() *object {
	return &object{m: make(map[string]any)}
}

func (o *object) set(key string, v any) {
	if _, ok := o.m[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.m[key] = v
}

func (o *object) delete(key string) {
	if _, ok := o.m[key]; !ok {
		return
	}
	delete(o.m, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			if len(o.keys) == 0 {
				o.keys = nil
			}
			return
		}
	}
}

func (o *object) get(key string) (any, bool) {
	v, ok := o.m[key]
	return v, ok
}

// parseSettings decodes one JSON document into ordered objects, rejecting
// duplicate keys anywhere in the document.
func parseSettings(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			seen := make(map[string]bool)
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				if seen[key] {
					return nil, fmt.Errorf("duplicate key %q", key)
				}
				seen[key] = true
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			var arr []any
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, errors.New("unexpected JSON delimiter")
	case string, json.Number, bool:
		return tok, nil
	case nil:
		return nil, nil
	}
	return nil, errors.New("unexpected JSON token")
}

// cloneDoc deep-copies a parsed document.
func cloneDoc(v any) any {
	switch x := v.(type) {
	case *object:
		o := newObject()
		for _, k := range x.keys {
			o.set(k, cloneDoc(x.m[k]))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneDoc(e)
		}
		return out
	default:
		return v
	}
}

// valuesEqual compares two decoded documents.
func valuesEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// envVals returns the env object Munch writes for endpoint.
func envVals(endpoint string) *object {
	o := newObject()
	vals := Env(endpoint)
	for _, k := range envNames {
		o.set(k, vals[k])
	}
	return o
}

// getEnv returns the env object of top, creating an empty one at the end of
// top when it is absent.
func getEnv(top *object) *object {
	if v, ok := top.get("env"); ok {
		if e, ok := v.(*object); ok {
			return e
		}
	}
	e := newObject()
	top.set("env", e)
	return e
}

// Inspect reads the Claude settings at path and plans what Apply must do so
// that Claude Desktop reports to the OTLP endpoint. It never writes. A file
// Munch must not touch is reported as Blocked, not as an error; errors are
// I/O failures.
func Inspect(path, endpoint string) (*Plan, error) {
	if !endpointRe.MatchString(endpoint) {
		return nil, errors.New("claudesettings: endpoint is not a munch route")
	}
	f, err := readSettings(path)
	if err != nil {
		return nil, err
	}
	p := &Plan{Path: path, file: f}
	block := func(reason string) (*Plan, error) {
		p.Status = Blocked
		p.Reason = reason
		return p, nil
	}

	var top *object
	switch {
	case f.unusable != "":
		return block(f.unusable)
	case f.content == "":
		top = newObject()
		p.Status = Unconfigured
	default:
		doc, err := parseSettings(f.content)
		if err != nil {
			return block("it is not valid JSON or has duplicate keys")
		}
		var ok bool
		top, ok = doc.(*object)
		if !ok {
			return block("the top level is not an object")
		}
		if _, ok := top.get("otelHeadersHelper"); ok {
			return block("it has an otelHeadersHelper setting")
		}
		var env *object
		if v, ok := top.get("env"); ok {
			e, isObj := v.(*object)
			if !isObj {
				return block("env is not an object")
			}
			for _, k := range e.keys {
				if _, ok := e.m[k].(string); !ok {
					return block("env has a value that is not a string")
				}
			}
			env = e
		}

		telemetry := false
		if env != nil {
			for _, k := range env.keys {
				if isTelemetryKey(k) {
					telemetry = true
					break
				}
			}
		}
		switch {
		case !telemetry:
			p.Status = Unconfigured
		default:
			epVal, ok := env.m[logsEndpointKey].(string)
			switch {
			case !ok:
				return block("it has other telemetry settings")
			case !endpointRe.MatchString(epVal):
				return block("it already exports telemetry somewhere else")
			}
			want := Env(epVal)
			for _, k := range env.keys {
				if !isTelemetryKey(k) {
					continue
				}
				w, ok := want[k]
				if !ok {
					return block("it has other telemetry settings")
				}
				if env.m[k] != w {
					return block("its telemetry settings were changed by hand")
				}
			}
			for k := range want {
				if _, ok := env.m[k]; !ok {
					return block("its telemetry settings are incomplete")
				}
			}
			if epVal == endpoint {
				p.Status = Current
			} else {
				p.Status = Stale
			}
		}
	}

	// The edited document must parse back to the original plus exactly the
	// intended change, which is what Apply will write.
	if p.Status != Current {
		want := cloneDoc(top).(*object)
		switch p.Status {
		case Unconfigured:
			e := getEnv(want)
			vals := envVals(endpoint)
			for _, k := range envNames {
				if _, ok := e.m[k]; !ok {
					e.set(k, vals.m[k])
				}
			}
		case Stale:
			getEnv(want).set(logsEndpointKey, endpoint)
		}
		newText := renderSettings(want, detectEOL(f.content))
		got, err := parseSettings(newText)
		if err != nil || !valuesEqual(want, got) {
			return block("munch cannot add its settings without changing others")
		}
		p.updated = newText
	}
	return p, nil
}

// Apply writes the edits of a plan. It does nothing when the plan makes none.
// The file is replaced atomically, through a temporary file next to the file
// a symlink points to, with mode 0600 for a new file because it holds the
// private route. Apply fails with ErrChanged, writing nothing, if the file
// changed after Inspect.
func Apply(p *Plan) error {
	if p.Status == Blocked {
		return errors.New("claudesettings: refusing to apply a blocked plan")
	}
	if !p.NeedsWrite() {
		return nil
	}
	perm := os.FileMode(0o600)
	if p.file.exists {
		perm = p.file.mode
	} else if err := os.MkdirAll(filepath.Dir(p.file.target), 0o700); err != nil {
		return err
	}
	return writeFile(p.file, p.updated, perm)
}

// Removal says what Remove did to a settings file.
type Removal struct {
	// Removed is set when Munch's telemetry keys were deleted from the file.
	Removed bool

	// Kept names telemetry keys left in place because their value does not
	// match what Munch wrote.
	Kept []string
}

// Remove deletes Munch's telemetry keys from env, but only when the env's
// endpoint is a Munch route and only keys whose value still reads exactly as
// Munch wrote them; differing keys are reported in Kept. env is dropped when
// it ends up empty. A missing or invalid file is a no-op.
func Remove(path string) (Removal, error) {
	var res Removal
	f, err := readSettings(path)
	if err != nil {
		return res, err
	}
	if !f.exists || f.unusable != "" {
		return res, nil
	}
	doc, err := parseSettings(f.content)
	if err != nil {
		return res, nil
	}
	top, ok := doc.(*object)
	if !ok {
		return res, nil
	}
	v, ok := top.get("env")
	if !ok {
		return res, nil
	}
	env, ok := v.(*object)
	if !ok {
		return res, nil
	}
	epVal, ok := env.m[logsEndpointKey].(string)
	if !ok || !endpointRe.MatchString(epVal) {
		return res, nil
	}
	want := Env(epVal)
	newEnv := cloneDoc(env).(*object)
	for _, k := range envNames {
		val, present := env.m[k]
		if present && val == want[k] {
			newEnv.delete(k)
		} else if present {
			res.Kept = append(res.Kept, k)
		}
	}
	newTop := cloneDoc(top).(*object)
	if len(newEnv.keys) == 0 {
		newTop.delete("env")
	} else {
		newTop.set("env", newEnv)
	}
	newText := renderSettings(newTop, detectEOL(f.content))
	if newText == f.content {
		return res, nil
	}
	got, err := parseSettings(newText)
	if err != nil || !valuesEqual(newTop, got) {
		return res, fmt.Errorf("munch could not remove its Claude telemetry settings from %s without changing others; delete them by hand", path)
	}
	if err := writeFile(f, newText, f.mode); err != nil {
		return res, err
	}
	res.Removed = true
	return res, nil
}

// detectEOL returns the line ending the file uses, so written JSON matches.
func detectEOL(text string) string {
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// quoteString returns s as a JSON string, without HTML escaping (<, >, & are
// written as-is), matching SetEscapeHTML(false).
func quoteString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u`)
				b.WriteByte(hex[r>>12&0xf])
				b.WriteByte(hex[r>>8&0xf])
				b.WriteByte(hex[r>>4&0xf])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// writeJSON writes v indented to b. Indentation is two spaces per depth level.
func writeJSON(b *strings.Builder, v any, depth int) {
	pad := func(n int) {
		for range n {
			b.WriteString("  ")
		}
	}
	switch x := v.(type) {
	case *object:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range x.keys {
			pad(depth + 1)
			b.WriteString(quoteString(k))
			b.WriteString(": ")
			writeJSON(b, x.m[k], depth+1)
			if i < len(x.keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		pad(depth)
		b.WriteString("}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			pad(depth + 1)
			writeJSON(b, e, depth+1)
			if i < len(x)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		pad(depth)
		b.WriteString("]")
	case string:
		b.WriteString(quoteString(x))
	case json.Number:
		b.WriteString(x.String())
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		panic(fmt.Sprintf("claudesettings: unexpected value of type %T", v))
	}
}

// renderSettings renders top as indented JSON ending with a newline, using the
// line ending the file already uses.
func renderSettings(top *object, eol string) string {
	var b strings.Builder
	writeJSON(&b, top, 0)
	s := b.String()
	if eol == "\r\n" {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s + eol
}

// writeFile atomically replaces the file behind f with content, through a
// temporary file in the same directory. It refuses, with ErrChanged, to
// replace a file that is no longer what f read.
func writeFile(f settingsFile, content string, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(f.target), ".munch-claude-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := verifyUnchanged(f); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, f.target); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// verifyUnchanged returns ErrChanged unless the file on disk is still what f
// read.
func verifyUnchanged(f settingsFile) error {
	cur, err := os.ReadFile(f.target)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if f.exists {
			return ErrChanged
		}
		return nil
	case err != nil:
		return err
	case !f.exists || string(cur) != f.content:
		return ErrChanged
	}
	return nil
}

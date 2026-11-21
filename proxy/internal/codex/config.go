package codex

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Codex reads its model provider from a user-level config.toml
// ($CODEX_HOME/config.toml, default ~/.codex/config.toml). munch routes Codex
// signed in with ChatGPT through the proxy by adding two marked blocks to it:
//
//	# >>> munch codex select >>>    model_provider = "munch_chatgpt", at the top of the file
//	# >>> munch codex provider >>>  the [model_providers.munch_chatgpt] table, at the end
//
// Every other byte of the file is left as it was. Edits are plain text
// insertions and deletions that are checked by parsing the result: the new
// document must equal the old one plus exactly the intended change, or nothing
// is written. Only blocks that still read exactly as munch wrote them are ever
// updated or removed; a provider the user wrote by hand is updated in place at
// most, and never removed.

const (
	providerID   = "munch_chatgpt"
	providerName = "Munch"

	selectStart = "# >>> munch codex select >>>"
	selectEnd   = "# <<< munch codex select <<<"
	tableStart  = "# >>> munch codex provider >>>"
	tableEnd    = "# <<< munch codex provider <<<"
	notice      = "# Managed by munch install; removed by munch uninstall."

	selectLine = `model_provider = "munch_chatgpt"`
	tableLine  = "[model_providers.munch_chatgpt]"
	nameLine   = `name = "Munch"`
	baseURLKey = `base_url = "`
	wireLine   = `wire_api = "responses"`
	authLine   = "requires_openai_auth = true"

	// maxConfigSize is the largest config.toml munch will read.
	maxConfigSize = 1 << 20

	// loopbackHost matches the hosts munch listens on, and localhost.
	loopbackHost = `(?:127(?:\.[0-9]{1,3}){3}|\[::1\]|localhost)`
)

var (
	// providerURLRe matches the base_url of a munch provider.
	providerURLRe = regexp.MustCompile(`^http://` + loopbackHost + `:[0-9]{1,5}/_tm/[0-9a-f]{64}/chatgpt/backend-api/codex$`)

	// legacyURLRe matches the plain-HTTP chatgpt_base_url the earlier manual
	// setup documented. Codex 0.171 and later reject it.
	legacyURLRe = regexp.MustCompile(`^http://` + loopbackHost + `:[0-9]{1,5}/_tm/[0-9a-f]{64}/chatgpt/backend-api/?$`)
)

// ErrChanged means the config file changed while munch was preparing its edit.
// Nothing was written.
var ErrChanged = errors.New("the Codex config changed while munch was editing it; run the command again")

// Status says how a Codex config relates to the proxy munch is setting up.
type Status int

const (
	// Unrouted: the config has no munch settings. Apply adds them.
	Unrouted Status = iota

	// Routed: Codex already goes through this proxy, by munch's own blocks or
	// by the same settings written by hand.
	Routed

	// Stale: a munch provider is present but points at another proxy address,
	// or only half of munch's blocks is left. Apply repoints or completes it.
	Stale

	// Blocked: the config holds something munch must not overwrite, or cannot
	// edit without changing other settings. Plan.Reason says what.
	Blocked
)

// Action is one edit Apply makes.
type Action int

const (
	// AddRoute writes munch's provider blocks.
	AddRoute Action = iota

	// UpdateRoute points an existing munch provider at this proxy.
	UpdateRoute

	// DropLegacyURL deletes a plain-HTTP munch chatgpt_base_url, which Codex
	// 0.171 and later reject.
	DropLegacyURL
)

// Plan is what Inspect found in a Codex config and what Apply will do about it.
type Plan struct {
	// Path is the config file as given to Inspect.
	Path string

	Status Status

	// Manual is set when the munch provider is the user's own hand-written
	// one. It is repointed in place if stale, and never removed.
	Manual bool

	// Reason says why Status is Blocked. It never contains the route URL or
	// any other value from the file.
	Reason string

	// Actions lists the edits Apply makes; empty when nothing needs to change.
	Actions []Action

	file    configFile
	updated string
}

// Has reports whether the plan includes a.
func (p *Plan) Has(a Action) bool {
	for _, x := range p.Actions {
		if x == a {
			return true
		}
	}
	return false
}

// NeedsWrite reports whether Apply will change the file.
func (p *Plan) NeedsWrite() bool { return len(p.Actions) > 0 }

// configFile is a config.toml as read from disk.
type configFile struct {
	// target is the file behind the path after following symlinks: the one
	// edits replace, so a symlinked config stays a symlink.
	target  string
	content string

	// mode holds the permission bits of an existing file.
	mode   os.FileMode
	exists bool

	// unusable is set when the file is not a regular file or is too large.
	// content is then empty.
	unusable string
}

func (f configFile) readOnly() bool { return f.exists && f.mode&0o200 == 0 }

// readConfig reads the config at path, following symlinks. A missing file
// reads as empty.
func readConfig(path string) (configFile, error) {
	target, err := resolve(path)
	if err != nil {
		return configFile{}, err
	}
	f := configFile{target: target}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return configFile{}, err
	}
	f.exists = true
	f.mode = info.Mode().Perm()
	switch {
	case !info.Mode().IsRegular():
		f.unusable = "it is not a regular file"
		return f, nil
	case info.Size() > maxConfigSize:
		f.unusable = "it is larger than 1 MiB"
		return f, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return configFile{}, err
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

// parse decodes a TOML document into generic values.
func parse(text string) (map[string]any, error) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// parseFailure describes a parse error without echoing any of the file.
func parseFailure(err error) string {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, _ := de.Position()
		return fmt.Sprintf("it is not valid TOML (error near line %d)", row)
	}
	return "it is not valid TOML"
}

// Inspect reads the Codex config at path and plans what Apply must do so that
// Codex goes through the proxy at baseURL (see BaseURL). It never writes. A
// config munch must not touch is reported as Blocked, not as an error; errors
// are I/O failures.
func Inspect(path, baseURL string) (*Plan, error) {
	if !providerURLRe.MatchString(baseURL) {
		return nil, errors.New("codex: baseURL is not a munch route")
	}
	f, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	p := &Plan{Path: path, file: f}
	block := func(reason string) (*Plan, error) {
		p.Status = Blocked
		p.Reason = reason
		p.Actions = nil
		return p, nil
	}

	switch {
	case f.unusable != "":
		return block(f.unusable)
	case f.readOnly():
		return block("it is read-only")
	case strings.HasPrefix(f.content, "\ufeff"):
		return block("it starts with a byte order mark")
	}
	doc, err := parse(f.content)
	if err != nil {
		return block(parseFailure(err))
	}

	// Ownership comes from the marker comments, meaning from the text; what the
	// settings mean comes from the parsed document.
	sel, selFound, selErr := findBlock(f.content, selectStart, selectEnd)
	tbl, tblFound, tblErr := findBlock(f.content, tableStart, tableEnd)
	if selErr != nil || tblErr != nil {
		return block("munch's marker comments in it do not pair up")
	}
	const edited = "a munch block in it was edited by hand"
	if selFound && !selectBodyOK(sel.body) {
		return block(edited)
	}
	tblURL := ""
	if tblFound {
		u, ok := tableBodyURL(tbl.body)
		if !ok {
			return block(edited)
		}
		tblURL = u
	}

	selected, hasSelected := doc["model_provider"]
	var providers map[string]any
	if v, ok := doc["model_providers"]; ok {
		if providers, ok = v.(map[string]any); !ok {
			return block("model_providers is not a table")
		}
	}
	existing, hasProvider := providers[providerID]

	legacy := ""
	if v, ok := doc["chatgpt_base_url"]; ok {
		s, _ := v.(string)
		switch {
		case isDefaultBackend(s):
		case legacyURLRe.MatchString(s):
			legacy = s
		default:
			return block("chatgpt_base_url points at a custom backend")
		}
	}

	if name, _ := doc["profile"].(string); name != "" {
		if profiles, ok := doc["profiles"].(map[string]any); ok {
			if profile, ok := profiles[name].(map[string]any); ok {
				if _, set := profile["model_provider"]; set {
					return block("the profile it selects sets its own model_provider")
				}
			}
		}
	}

	managed := selFound || tblFound
	current := "" // base_url of the munch provider already in the file
	switch {
	case managed:
		const misplaced = "a munch block in it is not where munch put it"
		if selFound && selected != providerID {
			return block(misplaced)
		}
		if tblFound {
			if u, ok := documentedProvider(existing); !ok || u != tblURL {
				return block(misplaced)
			}
		}
		if !selFound && hasSelected {
			return block("model_provider is set outside munch's block")
		}
		if !tblFound && hasProvider {
			return block("a munch_chatgpt provider exists outside munch's block")
		}
		current = tblURL
	case hasSelected:
		if selected != providerID {
			return block("model_provider is already set, so Codex uses another model provider")
		}
		u, ok := documentedProvider(existing)
		if !ok {
			return block("it has a munch_chatgpt provider that munch did not write and that differs from the documented setup")
		}
		p.Manual = true
		current = u
	case hasProvider:
		return block("it defines a munch_chatgpt provider that munch did not write")
	}

	switch {
	case !managed && !p.Manual:
		p.Status = Unrouted
	case (p.Manual || selFound && tblFound) && current == baseURL:
		p.Status = Routed
	default:
		p.Status = Stale
	}

	text := f.content
	eol := detectEOL(text)
	if legacy != "" {
		var ok bool
		if text, ok = dropLegacyLine(text, legacy); !ok {
			return block("it has an old munch chatgpt_base_url that munch could not remove on its own")
		}
		p.Actions = append(p.Actions, DropLegacyURL)
	}
	switch {
	case p.Status == Unrouted:
		if text, err = ensureBlocks(text, baseURL, eol); err != nil {
			return block("munch's marker comments in it do not pair up")
		}
		p.Actions = append(p.Actions, AddRoute)
	case p.Status == Stale && p.Manual:
		if strings.Count(text, current) != 1 {
			return block("it has a hand-written munch provider whose base_url munch could not update on its own")
		}
		text = strings.Replace(text, current, baseURL, 1)
		p.Actions = append(p.Actions, UpdateRoute)
	case p.Status == Stale:
		if text, err = ensureBlocks(text, baseURL, eol); err != nil {
			return block("munch's marker comments in it do not pair up")
		}
		p.Actions = append(p.Actions, UpdateRoute)
	}
	if len(p.Actions) == 0 {
		return p, nil
	}

	// The edited document must be the original plus exactly the intended
	// change. Classification is done, so the parsed original becomes that
	// expectation.
	want := doc
	if p.Status != Routed {
		setRoute(want, baseURL)
	}
	if legacy != "" {
		delete(want, "chatgpt_base_url")
	}
	got, err := parse(text)
	if err != nil || !valuesEqual(want, got) {
		return block("munch cannot add its settings without changing others (for example when model_providers is an inline table)")
	}
	p.updated = text
	return p, nil
}

// Apply writes the edits of a plan. It does nothing when the plan makes none.
// The file is replaced atomically, through a temporary file next to the file a
// symlink points to, with mode 0600 because it holds the private route. Apply
// fails with ErrChanged, writing nothing, if the file changed after Inspect.
func Apply(p *Plan) error {
	if p.Status == Blocked {
		return errors.New("codex: refusing to apply a blocked plan")
	}
	if !p.NeedsWrite() {
		return nil
	}
	return writeFile(p.file, p.updated, 0o600)
}

// Removal says what Remove did to a Codex config.
type Removal struct {
	// Removed is set when munch's own blocks were deleted from the file.
	Removed bool

	// Kept describes munch settings that were left in place because munch did
	// not write them or they were edited. Codex may still point at the proxy.
	Kept []string
}

// Remove deletes the blocks Apply wrote, but only those that still read
// exactly as munch wrote them, and reports every other munch setting it
// leaves behind. The provider block stays for as long as something still
// selects the provider, so Codex is never left pointing at a provider that is
// gone. A missing file is not an error and a config without munch settings is
// not touched. A config that ends up empty stays as an empty file: nothing
// records whether it existed before munch, and an empty config is harmless.
func Remove(path string) (Removal, error) {
	var res Removal
	f, err := readConfig(path)
	if err != nil {
		return res, err
	}
	if !f.exists || f.unusable != "" {
		return res, nil
	}
	doc, err := parse(f.content)
	if err != nil {
		if strings.Contains(f.content, providerID) || strings.Contains(f.content, "munch codex") {
			return res, fmt.Errorf("%s is not valid TOML, so munch cannot tell which Codex settings are its own; delete its blocks (the ones marked \"munch codex\") by hand", path)
		}
		return res, nil
	}

	pairing := fmt.Errorf("%s has munch comment markers that do not pair up; delete the blocks marked \"munch codex\" by hand", path)
	sel, selFound, err := findBlock(f.content, selectStart, selectEnd)
	if err != nil {
		return res, pairing
	}
	tbl, tblFound, err := findBlock(f.content, tableStart, tableEnd)
	if err != nil {
		return res, pairing
	}

	// A block is munch's to remove only while it still reads as munch wrote it.
	selOK := selFound && selectBodyOK(sel.body)
	tblOK := false
	if tblFound {
		_, tblOK = tableBodyURL(tbl.body)
	}
	// A selection that survives the removal of munch's own selection block
	// keeps the provider in use.
	selected, _ := doc["model_provider"].(string)
	stillSelected := selected == providerID && !selOK
	removeSel := selOK
	removeTbl := tblOK && !stillSelected
	edited := (selFound && !selOK) || (tblFound && !tblOK)

	// Planning is done, so the parsed original becomes what the file must
	// still say once the blocks are gone.
	want := doc
	text := f.content
	if removeTbl {
		text = removeBlock(text, tbl)
		if providers, ok := want["model_providers"].(map[string]any); ok {
			delete(providers, providerID)
		}
	}
	if removeSel {
		// The provider block, if it came first, moved this one.
		b, _, err := findBlock(text, selectStart, selectEnd)
		if err != nil {
			return res, pairing
		}
		text = removeBlock(text, b)
		delete(want, "model_provider")
	}

	got, err := parse(text)
	dropEmptyProviders(want)
	if err == nil {
		dropEmptyProviders(got)
	}
	if err != nil || !valuesEqual(want, got) {
		return res, fmt.Errorf("munch could not remove its Codex settings from %s without changing others; delete the blocks marked \"munch codex\" by hand", path)
	}

	if edited {
		res.Kept = append(res.Kept, "a munch block that was edited by hand")
	}
	if s, _ := got["model_provider"].(string); s == providerID {
		res.Kept = append(res.Kept, `model_provider = "munch_chatgpt"`)
	}
	if providers, ok := got["model_providers"].(map[string]any); ok {
		if _, ok := providers[providerID]; ok {
			res.Kept = append(res.Kept, "the [model_providers.munch_chatgpt] table")
		}
	}
	if s, _ := got["chatgpt_base_url"].(string); legacyURLRe.MatchString(s) {
		res.Kept = append(res.Kept, "chatgpt_base_url pointing at the munch proxy")
	}

	if text == f.content {
		return res, nil
	}
	if f.readOnly() {
		return res, fmt.Errorf("%s is read-only, so munch left its Codex settings in it", path)
	}
	if err := writeFile(f, text, f.mode); err != nil {
		return res, err
	}
	res.Removed = true
	return res, nil
}

// region is a marked region of a config. [start, end) covers both marker lines
// and everything between them, newline included.
type region struct {
	start, end int

	// body holds the lines between the markers, without line endings.
	body []string
}

// findBlock locates the region between the marker lines open and closing. found
// is false when neither marker is present; an error means they do not pair up.
func findBlock(text, open, closing string) (b region, found bool, err error) {
	opens, closes := 0, 0
	for off := 0; off < len(text); {
		next := len(text)
		line := text[off:]
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			next = off + nl + 1
			line = line[:nl]
		}
		switch strings.TrimRight(line, "\r") {
		case open:
			opens++
			b.start = off
		case closing:
			closes++
			b.end = next
		}
		off = next
	}
	switch {
	case opens == 0 && closes == 0:
		return region{}, false, nil
	case opens != 1 || closes != 1 || b.end <= b.start:
		return region{}, false, errors.New("munch's markers do not pair up")
	}
	lines := strings.Split(strings.TrimSuffix(text[b.start:b.end], "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	b.body = lines[1 : len(lines)-1]
	return b, true, nil
}

// selectBodyOK reports whether the lines inside the selection block are
// exactly what munch wrote.
func selectBodyOK(body []string) bool {
	return len(body) == 2 && body[0] == notice && body[1] == selectLine
}

// tableBodyURL returns the base_url of a provider block whose lines are
// exactly what munch wrote, and false when the block was edited.
func tableBodyURL(body []string) (string, bool) {
	if len(body) != 6 || body[0] != notice || body[1] != tableLine || body[2] != nameLine ||
		body[4] != wireLine || body[5] != authLine {
		return "", false
	}
	line := body[3]
	if len(line) <= len(baseURLKey) || !strings.HasPrefix(line, baseURLKey) || !strings.HasSuffix(line, `"`) {
		return "", false
	}
	u := line[len(baseURLKey) : len(line)-1]
	return u, providerURLRe.MatchString(u)
}

// documentedProvider returns the base_url of v when v is exactly the provider
// the documentation and munch's block define: those four settings and nothing
// else, with a munch route as base_url.
func documentedProvider(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 4 {
		return "", false
	}
	name, _ := m["name"].(string)
	wire, _ := m["wire_api"].(string)
	auth, _ := m["requires_openai_auth"].(bool)
	u, _ := m["base_url"].(string)
	if name != providerName || wire != "responses" || !auth || !providerURLRe.MatchString(u) {
		return "", false
	}
	return u, true
}

// isDefaultBackend reports whether s is Codex's default chatgpt_base_url.
func isDefaultBackend(s string) bool {
	return strings.TrimRight(s, "/") == "https://chatgpt.com/backend-api"
}

func renderSelect(eol string) string {
	return selectStart + eol + notice + eol + selectLine + eol + selectEnd + eol
}

func renderTable(baseURL, eol string) string {
	lines := []string{
		tableStart, notice, tableLine, nameLine,
		baseURLKey + baseURL + `"`,
		wireLine, authLine, tableEnd,
	}
	return strings.Join(lines, eol) + eol
}

// detectEOL returns the line ending the file uses, so inserted lines match.
func detectEOL(text string) string {
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// ensureBlocks returns text with both blocks present and current. An existing
// provider block is replaced in place. A missing selection block goes at the
// top, because a root-level key has to come before any table; a missing
// provider block goes at the end. One blank line separates each added block
// from the content next to it.
func ensureBlocks(text, baseURL, eol string) (string, error) {
	tbl, found, err := findBlock(text, tableStart, tableEnd)
	if err != nil {
		return "", err
	}
	if found {
		text = text[:tbl.start] + renderTable(baseURL, eol) + text[tbl.end:]
	} else {
		text = appendBlock(text, renderTable(baseURL, eol), eol)
	}
	_, found, err = findBlock(text, selectStart, selectEnd)
	if err != nil {
		return "", err
	}
	if !found {
		text = prependBlock(text, renderSelect(eol), eol)
	}
	return text, nil
}

func appendBlock(text, b, eol string) string {
	if text == "" {
		return b
	}
	if !strings.HasSuffix(text, "\n") {
		text += eol
	}
	return text + eol + b
}

func prependBlock(text, b, eol string) string {
	if text == "" {
		return b
	}
	return b + eol + text
}

// removeBlock deletes b and the one blank line install put beside it: the one
// before it or, for a block at the top of the file, the one after it.
func removeBlock(text string, b region) string {
	from, to := b.start, b.end
	if start, ok := blankLineBefore(text, from); ok {
		from = start
	} else if end, ok := blankLineAfter(text, to); ok {
		to = end
	}
	return text[:from] + text[to:]
}

// blankLineBefore reports whether the line that ends just before offset pos is
// blank, and where that line starts.
func blankLineBefore(text string, pos int) (int, bool) {
	if pos < 1 || text[pos-1] != '\n' {
		return pos, false
	}
	start := strings.LastIndexByte(text[:pos-1], '\n') + 1
	if strings.TrimRight(text[start:pos-1], "\r") != "" {
		return pos, false
	}
	return start, true
}

// blankLineAfter reports whether the line starting at offset pos is blank, and
// where the line after it starts.
func blankLineAfter(text string, pos int) (int, bool) {
	if pos >= len(text) {
		return pos, false
	}
	line, end := text[pos:], len(text)
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line, end = line[:nl], pos+nl+1
	}
	if strings.TrimRight(line, "\r") != "" {
		return pos, false
	}
	return end, true
}

// dropLegacyLine removes the one line that sets chatgpt_base_url to value. It
// reports false unless exactly one such line exists.
func dropLegacyLine(text, value string) (string, bool) {
	q := regexp.QuoteMeta(value)
	re := regexp.MustCompile(`(?m)^[ \t]*chatgpt_base_url[ \t]*=[ \t]*(?:"` + q + `"|'` + q + `')[ \t]*(?:#[^\r\n]*)?\r?(?:\n|\z)`)
	locs := re.FindAllStringIndex(text, -1)
	if len(locs) != 1 {
		return text, false
	}
	return text[:locs[0][0]] + text[locs[0][1]:], true
}

// setRoute makes doc what the document looks like once the munch provider is
// selected and defined.
func setRoute(doc map[string]any, baseURL string) {
	doc["model_provider"] = providerID
	providers, _ := doc["model_providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		doc["model_providers"] = providers
	}
	providers[providerID] = map[string]any{
		"name":                 providerName,
		"base_url":             baseURL,
		"wire_api":             "responses",
		"requires_openai_auth": true,
	}
}

// dropEmptyProviders removes an empty model_providers table, which a document
// without any provider table reads the same as.
func dropEmptyProviders(doc map[string]any) {
	if providers, ok := doc["model_providers"].(map[string]any); ok && len(providers) == 0 {
		delete(doc, "model_providers")
	}
}

// valuesEqual compares two decoded TOML values.
func valuesEqual(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !valuesEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valuesEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || (math.IsNaN(x) && math.IsNaN(y)))
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	default:
		return reflect.DeepEqual(a, b)
	}
}

// writeFile atomically replaces the file behind f with content, through a
// temporary file in the same directory. It refuses, with ErrChanged, to
// replace a file that is no longer what f read.
func writeFile(f configFile, content string, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(f.target), ".munch-codex-*")
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
func verifyUnchanged(f configFile) error {
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

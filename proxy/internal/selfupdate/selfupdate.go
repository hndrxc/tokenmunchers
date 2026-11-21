// Package selfupdate replaces the running munch binary with the newest stable
// release published on GitHub.
//
// Update fetches only from the official release URLs, over HTTPS, and checks
// the archive against the release's SHA256SUMS before reading it. The new
// binary is validated from its header and never run, then swapped in with one
// rename inside the executable's own directory, so a failure at any step
// leaves the installed binary exactly as it was. Update never touches the
// background service, credentials, or configuration.
package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The release contract is the one install-munch.sh follows: the newest
// published munch-vX.Y.Z release of the repository, its platform archive, and
// its SHA256SUMS. The URLs are fixed; nothing in the environment, flags, or
// configuration can change where an update comes from.
const (
	releasesURL = "https://api.github.com/repos/hndrxc/tokenmunchers/releases"
	downloadURL = "https://github.com/hndrxc/tokenmunchers/releases/download"

	tagPrefix  = "munch-v"
	binaryName = "munch"
	sumsName   = "SHA256SUMS"

	pageSize     = 100 // releases per API page, GitHub's maximum
	maxPages     = 30  // give up on a list that never ends
	maxRedirects = 5
)

// Time and size bounds. The release list and SHA256SUMS are read into memory;
// the archive goes to a temporary file. Each response has a cap.
const (
	totalTimeout   = 10 * time.Minute
	apiTimeout     = 30 * time.Second
	sumsTimeout    = 30 * time.Second
	archiveTimeout = 5 * time.Minute

	maxAPIBytes     = 16 << 20
	maxSumsBytes    = 64 << 10
	maxArchiveBytes = 64 << 20
	maxExtracted    = 256 << 20 // decompressed archive, headers included
)

var (
	// ErrDevelopmentBuild means the running binary has no release version, so
	// there is nothing to compare a release against.
	ErrDevelopmentBuild = errors.New("munch update only works on release builds, and this is a development build")

	// ErrUnsupportedPlatform means no release archive exists for this system.
	ErrUnsupportedPlatform = errors.New("munch update supports linux and darwin on amd64 and arm64 only")
)

var (
	apiHeaders = map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	// Assets are hashed as stored, so ask for them without transfer
	// compression.
	assetHeaders = map[string]string{
		"Accept-Encoding": "identity",
	}
)

// Options describes the running build and how to reach the release server.
type Options struct {
	// Version is the running build's release version, MAJOR.MINOR.PATCH with
	// no "v" prefix. Anything else, such as "dev", is a development build and
	// is refused.
	Version string

	// GOOS and GOARCH select the release archive to install.
	GOOS, GOARCH string

	// Executable is the path of the running binary. Symbolic links are
	// resolved and the regular file they lead to is replaced.
	Executable string

	// Transport carries every HTTP request. It is nil, meaning the default
	// transport, everywhere except tests, which use it to serve the official
	// URLs from a fixture.
	Transport http.RoundTripper

	// Logf, if set, receives a line for each step that takes a while.
	Logf func(format string, args ...any)
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Result reports what Update found and did.
type Result struct {
	Current string // version that was running
	Latest  string // newest stable release published
	Path    string // executable that was replaced; empty if Updated is false
	Updated bool   // the executable now holds Latest
}

// Update installs the newest stable release over the running executable. It
// never downgrades and never reinstalls the running version: when the running
// version is at least as new as every stable release, Update changes nothing
// and returns a Result with Updated false.
func Update(ctx context.Context, opts Options) (Result, error) {
	current, ok := parseVersion(opts.Version)
	if !ok {
		return Result{}, fmt.Errorf("%w (version %q)", ErrDevelopmentBuild, opts.Version)
	}
	if !supportedPlatform(opts.GOOS, opts.GOARCH) {
		return Result{}, fmt.Errorf("%w (this is %s/%s)", ErrUnsupportedPlatform, opts.GOOS, opts.GOARCH)
	}
	tgt, err := resolveTarget(opts.Executable)
	if err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	f := &fetcher{
		client:    &http.Client{Transport: opts.Transport, CheckRedirect: requireHTTPS},
		userAgent: "munch-update/" + current.String(),
	}

	latest, err := latestRelease(ctx, f)
	if err != nil {
		return Result{}, err
	}
	res := Result{Current: current.String(), Latest: latest.String()}
	if latest.compare(current) <= 0 {
		return res, nil
	}

	// Stage first: a directory munch cannot write to fails here, before any
	// download.
	stage, err := newStaged(tgt)
	if err != nil {
		return Result{}, err
	}
	defer stage.discard()

	archiveName := fmt.Sprintf("munch-%s-%s-%s.tar.gz", res.Latest, opts.GOOS, opts.GOARCH)
	base := downloadURL + "/" + tagPrefix + res.Latest + "/"
	opts.logf("downloading munch %s for %s/%s", res.Latest, opts.GOOS, opts.GOARCH)

	sums, err := f.get(ctx, base+sumsName, sumsName, sumsTimeout, maxSumsBytes, assetHeaders)
	if err != nil {
		return Result{}, err
	}
	want, err := digestFor(sums, archiveName)
	if err != nil {
		return Result{}, err
	}
	scratch, err := newScratch()
	if err != nil {
		return Result{}, err
	}
	defer scratch.Close()
	got, err := f.download(ctx, base+archiveName, archiveName, archiveTimeout, maxArchiveBytes, assetHeaders, scratch)
	if err != nil {
		return Result{}, err
	}
	if got != want {
		return Result{}, fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	opts.logf("verified the SHA-256 checksum of %s", archiveName)

	// Only an archive that matched its digest is ever read.
	if _, err := scratch.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("rewind the download: %w", err)
	}
	if err := extractBinary(stage, scratch, opts.GOOS, opts.GOARCH); err != nil {
		return Result{}, fmt.Errorf("%s: %w", archiveName, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := stage.install(tgt); err != nil {
		return Result{}, err
	}
	res.Path, res.Updated = tgt.path, true
	return res, nil
}

func supportedPlatform(goos, goarch string) bool {
	return (goos == "linux" || goos == "darwin") && (goarch == "amd64" || goarch == "arm64")
}

// version is a MAJOR.MINOR.PATCH release number.
type version [3]uint64

// parseVersion accepts exactly MAJOR.MINOR.PATCH, the form release tags and
// the linked-in build version use: decimal numbers without leading zeros.
func parseVersion(s string) (version, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		if len(p) > 1 && p[0] == '0' {
			return version{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

// parseTag accepts exactly munch-vMAJOR.MINOR.PATCH.
func parseTag(tag string) (version, bool) {
	s, ok := strings.CutPrefix(tag, tagPrefix)
	if !ok {
		return version{}, false
	}
	return parseVersion(s)
}

func (v version) compare(w version) int {
	for i := range v {
		switch {
		case v[i] < w[i]:
			return -1
		case v[i] > w[i]:
			return 1
		}
	}
	return 0
}

func (v version) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

type listedRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// latestRelease returns the highest munch-vX.Y.Z version among the published,
// non-prerelease releases of the repository. GitHub lists releases newest
// first, which is not version order (a patch to an old line comes after a
// newer line), so it reads the whole list rather than trusting the first match.
func latestRelease(ctx context.Context, f *fetcher) (version, error) {
	var best version
	found := false
	for page := 1; page <= maxPages; page++ {
		pageURL := fmt.Sprintf("%s?per_page=%d&page=%d", releasesURL, pageSize, page)
		body, err := f.get(ctx, pageURL, "list releases", apiTimeout, maxAPIBytes, apiHeaders)
		if err != nil {
			return version{}, err
		}
		var releases []listedRelease
		if err := json.Unmarshal(body, &releases); err != nil || releases == nil {
			return version{}, errors.New("list releases: unexpected response from the GitHub API")
		}
		if len(releases) == 0 {
			if !found {
				return version{}, errors.New("no stable munch release found")
			}
			return best, nil
		}
		for _, r := range releases {
			if r.Draft || r.Prerelease {
				continue
			}
			if v, ok := parseTag(r.TagName); ok && (!found || v.compare(best) > 0) {
				best, found = v, true
			}
		}
	}
	return version{}, fmt.Errorf("list releases: the list did not end within %d pages", maxPages)
}

// fetcher performs the bounded HTTPS GETs of an update.
type fetcher struct {
	client    *http.Client
	userAgent string
}

// open starts a GET of rawURL and returns the response if it is a 200 that
// declares no more than limit bytes. The caller reads and closes its body
// within ctx. what names the request in errors, which never contain the URL:
// after a redirect it is a signed download address.
func (f *fetcher) open(ctx context.Context, rawURL, what string, limit int64, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, withoutURL(err))
	}
	req.Header.Set("User-Agent", f.userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, withoutURL(err))
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%s: HTTP %d (rate limited? try again later)", what, resp.StatusCode)
		}
		return nil, fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: response of %d bytes exceeds the %d byte limit", what, resp.ContentLength, limit)
	}
	return resp, nil
}

// get returns the body of a 200 response to GET rawURL. The request, including
// reading the body, must finish within timeout, and the body may not exceed
// limit bytes. It is for small responses: the body is held in memory.
func (f *fetcher) get(ctx context.Context, rawURL, what string, timeout time.Duration, limit int64, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := f.open(ctx, rawURL, what, limit, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, limit, resp.ContentLength)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return body, nil
}

// download is get for large responses: the body goes to dst as it arrives and
// is never held in memory. It returns the hex SHA-256 digest of what it wrote.
// The same time and size limits apply.
func (f *fetcher) download(ctx context.Context, rawURL, what string, timeout time.Duration, limit int64, headers map[string]string, dst io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := f.open(ctx, rawURL, what, limit, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if n > limit {
		return "", fmt.Errorf("%s: response exceeds the %d byte limit", what, limit)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readLimited reads r to its end and fails if that takes more than limit
// bytes. size is the expected length, or <= 0 if unknown; it only presizes the
// buffer.
func readLimited(r io.Reader, limit, size int64) ([]byte, error) {
	var buf bytes.Buffer
	if size > 0 && size <= limit {
		buf.Grow(int(size) + bytes.MinRead)
	}
	if _, err := buf.ReadFrom(io.LimitReader(r, limit+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > limit {
		return nil, fmt.Errorf("response exceeds the %d byte limit", limit)
	}
	return buf.Bytes(), nil
}

// requireHTTPS is the redirect policy: every hop must be HTTPS, and there may
// not be many.
func requireHTTPS(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("refusing a redirect to a non-HTTPS address")
	}
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// withoutURL strips the *url.Error wrapper net/http puts around failures,
// leaving the cause.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	apiRoute  = "api.github.com/repos/hndrxc/tokenmunchers/releases"
	cdnHost   = "release-assets.githubusercontent.com"
	oldBinary = "old munch"
)

func downloadRoute(ver, file string) string {
	return "github.com/hndrxc/tokenmunchers/releases/download/munch-v" + ver + "/" + file
}

func archiveFile(ver, goos, goarch string) string {
	return fmt.Sprintf("munch-%s-%s-%s.tar.gz", ver, goos, goarch)
}

// fixture serves GitHub's release endpoints from memory, over plain HTTP. As a
// RoundTripper it carries the updater's requests, which are for the official
// HTTPS URLs, to itself, keeping the original host so routes are "host/path".
// Tests replace routes to inject failures.
type fixture struct {
	srv *httptest.Server

	mu     sync.Mutex
	seen   []string // host and URI of each request, in order
	routes map[string]http.HandlerFunc
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

func (f *fixture) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Host+r.URL.RequestURI())
	h := f.routes[r.Host+r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

// RoundTrip refuses anything but HTTPS, which the updater must never send.
func (f *fixture) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("fixture: refusing a %s request to %s", req.URL.Scheme, req.URL.Host)
	}
	out := req.Clone(req.Context())
	out.Host = req.URL.Host
	out.URL.Scheme = "http"
	out.URL.Host = f.srv.Listener.Addr().String()
	return f.srv.Client().Transport.RoundTrip(out)
}

func stable(tag string) string     { return fmt.Sprintf(`{"tag_name":%q}`, tag) }
func draft(tag string) string      { return fmt.Sprintf(`{"tag_name":%q,"draft":true}`, tag) }
func prerelease(tag string) string { return fmt.Sprintf(`{"tag_name":%q,"prerelease":true}`, tag) }

func page(releases ...string) string { return "[" + strings.Join(releases, ",") + "]" }

// listPages serves the release list: page i+1 is pages[i], and every later
// page is empty. It insists on the request GitHub's API expects.
func (f *fixture) listPages(pages ...string) {
	f.handle(apiRoute, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Header.Get("Accept") != "application/vnd.github+json" ||
			r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" ||
			!strings.HasPrefix(r.Header.Get("User-Agent"), "munch-update/") ||
			q.Get("per_page") != "100" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		n, _ := strconv.Atoi(q.Get("page"))
		if n >= 1 && n <= len(pages) {
			io.WriteString(w, pages[n-1])
			return
		}
		io.WriteString(w, "[]")
	})
}

func sumLine(file string, content []byte) string {
	return fmt.Sprintf("%x  %s\n", sha256.Sum256(content), file)
}

// sumsFor is a SHA256SUMS listing file's digest among unrelated entries.
func sumsFor(file string, archive []byte) string {
	return sumLine("munch-0.0.1-other.tar.gz", []byte("other")) +
		sumLine(file, archive) +
		sumLine("munch-0.0.2-other.tar.gz", []byte("another"))
}

// release serves SHA256SUMS and the archive of a release, the archive through a
// redirect to a CDN host as GitHub does.
func (f *fixture) release(ver, goos, goarch string, archive []byte) {
	file := archiveFile(ver, goos, goarch)
	f.handle(downloadRoute(ver, "SHA256SUMS"), func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sumsFor(file, archive))
	})
	f.handle(downloadRoute(ver, file), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+cdnHost+"/asset/"+file, http.StatusFound)
	})
	f.handle(cdnHost+"/asset/"+file, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
}

// fakeBinary is a stand-in executable for goos/goarch: the header the updater
// inspects, then payload. The field values are the documented ones: ELF
// e_machine 62 (x86-64) and 183 (AArch64); Mach-O cputype 0x01000007 and
// 0x0100000c, filetype 2 (executable).
func fakeBinary(goos, goarch, payload string) []byte {
	var head []byte
	switch goos {
	case "linux":
		head = make([]byte, 64)
		copy(head, "\x7fELF")
		head[4], head[5], head[6] = 2, 1, 1 // 64-bit, little-endian, version 1
		binary.LittleEndian.PutUint16(head[16:], 2)
		binary.LittleEndian.PutUint16(head[18:], map[string]uint16{"amd64": 62, "arm64": 183}[goarch])
	case "darwin":
		head = make([]byte, 32)
		binary.LittleEndian.PutUint32(head[0:], 0xfeedfacf)
		binary.LittleEndian.PutUint32(head[4:], map[string]uint32{"amd64": 0x01000007, "arm64": 0x0100000c}[goarch])
		binary.LittleEndian.PutUint32(head[12:], 2)
	}
	return append(head, payload...)
}

// randomPayload is n bytes of deterministic noise (a SHA-256 chain). It does
// not compress, so an archive holding it is about as large as the payload.
func randomPayload(n int) string {
	var out []byte
	sum := sha256.Sum256([]byte("munch update test payload"))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return string(out[:n])
}

type tarEntry struct {
	name     string
	typeflag byte
	body     []byte
	link     string
}

func binaryEntry(body []byte) tarEntry {
	return tarEntry{name: "munch", typeflag: tar.TypeReg, body: body}
}

// makeArchive builds a gzipped tar of entries, in order.
func makeArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Linkname: e.link, Mode: 0o755}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gzipOf compresses data into one gzip stream.
func gzipOf(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile applies the umask
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// env is an installed binary, a release server, and a scratch TMPDIR.
type env struct {
	t    *testing.T
	fx   *fixture
	dir  string      // directory of the installed binary
	exe  string      // the installed binary
	tmp  string      // TMPDIR: where a download would be stored
	mode os.FileMode // permission bits of the installed binary
	opts Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, fx: newFixture(t), mode: 0o755}
	e.dir = realTempDir(t)
	e.exe = filepath.Join(e.dir, "munch")
	writeFile(t, e.exe, []byte(oldBinary), e.mode)
	e.tmp = realTempDir(t)
	t.Setenv("TMPDIR", e.tmp)
	e.opts = Options{Version: "1.0.0", GOOS: "linux", GOARCH: "amd64", Executable: e.exe, Transport: e.fx}
	return e
}

func (e *env) update() (Result, error) {
	e.t.Helper()
	return Update(context.Background(), e.opts)
}

// serveArchive publishes ver as the only stable release, with archive as the
// download for the env's platform.
func (e *env) serveArchive(ver string, archive []byte) {
	e.t.Helper()
	e.fx.listPages(page(stable("munch-v" + ver)))
	e.fx.release(ver, e.opts.GOOS, e.opts.GOARCH, archive)
}

// serveRelease publishes a good release of ver and returns the binary in it.
func (e *env) serveRelease(ver string) []byte {
	e.t.Helper()
	bin := fakeBinary(e.opts.GOOS, e.opts.GOARCH, "munch "+ver)
	e.serveArchive(ver, makeArchive(e.t, binaryEntry(bin)))
	return bin
}

func (e *env) assertNoLeftovers() {
	e.t.Helper()
	if names := dirNames(e.t, e.dir); !reflect.DeepEqual(names, []string{"munch"}) {
		e.t.Errorf("unexpected files in %s: %v", e.dir, names)
	}
	if names := dirNames(e.t, e.tmp); len(names) != 0 {
		e.t.Errorf("temporary files left in TMPDIR: %v", names)
	}
}

// assertIntact checks the installed binary is exactly as it was and nothing
// was left behind, in its directory or in TMPDIR.
func (e *env) assertIntact() {
	e.t.Helper()
	got, err := os.ReadFile(e.exe)
	if err != nil || string(got) != oldBinary {
		e.t.Errorf("installed binary changed: %q (err=%v)", got, err)
	}
	if info, err := os.Stat(e.exe); err != nil || info.Mode().Perm() != e.mode {
		e.t.Errorf("installed binary permissions changed: %v (err=%v)", info, err)
	}
	e.assertNoLeftovers()
}

func TestUpdateInstallsHighestStableRelease(t *testing.T) {
	for _, p := range []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"},
	} {
		t.Run(p.goos+"-"+p.goarch, func(t *testing.T) {
			e := newEnv(t)
			e.opts.GOOS, e.opts.GOARCH = p.goos, p.goarch
			want := fakeBinary(p.goos, p.goarch, "munch 1.10.0")

			// GitHub lists newest first, which is not version order. 1.10.0 is
			// the highest release (numerically, not as text), and it sits on
			// the second page behind everything that must not be chosen.
			e.fx.listPages(
				page(
					draft("munch-v9.9.9"),
					prerelease("munch-v8.0.0"),
					stable("v0.2.0"),
					stable("munch-v1.9.9"),
					stable("munch-v1.2"),
					stable("munch-v1.2.3.4"),
					stable("munch-v01.2.3"),
					stable("munch-v7.0.0-rc1"),
					stable("xmunch-v7.0.0"),
					stable("munch-v99999999999999999999.0.0"),
					stable("munch-v1.2.3\n"),
				),
				page(stable("munch-v1.10.0"), stable("munch-v1.0.0")),
			)
			e.fx.release("1.10.0", p.goos, p.goarch, makeArchive(t, binaryEntry(want)))
			var logged []string
			e.opts.Logf = func(format string, args ...any) {
				logged = append(logged, fmt.Sprintf(format, args...))
			}

			res, err := e.update()
			if err != nil {
				t.Fatal(err)
			}
			if res != (Result{Current: "1.0.0", Latest: "1.10.0", Path: e.exe, Updated: true}) {
				t.Errorf("result = %+v", res)
			}
			if got, err := os.ReadFile(e.exe); err != nil || !bytes.Equal(got, want) {
				t.Errorf("installed binary = %q (err=%v), want the release's binary", got, err)
			}
			if info, err := os.Stat(e.exe); err != nil || info.Mode().Perm() != 0o755 {
				t.Errorf("permissions = %v (err=%v)", info, err)
			}
			e.assertNoLeftovers()
			if len(logged) == 0 || !strings.Contains(logged[0], "1.10.0") {
				t.Errorf("progress lines = %q", logged)
			}

			// Every request went to an official URL: the whole list, then
			// SHA256SUMS, then the archive and its CDN redirect.
			file := archiveFile("1.10.0", p.goos, p.goarch)
			wantRequests := []string{
				"api.github.com/repos/hndrxc/tokenmunchers/releases?per_page=100&page=1",
				"api.github.com/repos/hndrxc/tokenmunchers/releases?per_page=100&page=2",
				"api.github.com/repos/hndrxc/tokenmunchers/releases?per_page=100&page=3",
				"github.com/hndrxc/tokenmunchers/releases/download/munch-v1.10.0/SHA256SUMS",
				"github.com/hndrxc/tokenmunchers/releases/download/munch-v1.10.0/" + file,
				"release-assets.githubusercontent.com/asset/" + file,
			}
			if got := e.fx.requests(); !reflect.DeepEqual(got, wantRequests) {
				t.Errorf("requests:\n got %v\nwant %v", got, wantRequests)
			}
		})
	}
}

func TestUpdatePreservesPermissionBits(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o700, 0o750, 0o711, 0o555} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			e := newEnv(t)
			writeFile(t, e.exe, []byte(oldBinary), mode)
			want := e.serveRelease("1.1.0")

			if _, err := e.update(); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(e.exe); err != nil || !bytes.Equal(got, want) {
				t.Errorf("installed binary = %q (err=%v)", got, err)
			}
			if info, err := os.Stat(e.exe); err != nil || info.Mode().Perm() != mode {
				t.Errorf("permissions = %v (err=%v), want %o", info, err, mode)
			}
		})
	}
}

func TestUpdateReplacesTheFileBehindASymlink(t *testing.T) {
	e := newEnv(t)
	linkDir := realTempDir(t)
	link := filepath.Join(linkDir, "munch")
	if err := os.Symlink(e.exe, link); err != nil {
		t.Fatal(err)
	}
	e.opts.Executable = link
	want := e.serveRelease("1.1.0")

	res, err := e.update()
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != e.exe || !res.Updated {
		t.Errorf("result = %+v, want the file behind the link (%s)", res, e.exe)
	}
	if got, err := os.ReadFile(e.exe); err != nil || !bytes.Equal(got, want) {
		t.Errorf("file behind the link = %q (err=%v), want the release's binary", got, err)
	}
	if dest, err := os.Readlink(link); err != nil || dest != e.exe {
		t.Errorf("symlink = %q (err=%v), want it unchanged", dest, err)
	}
	if names := dirNames(t, linkDir); !reflect.DeepEqual(names, []string{"munch"}) {
		t.Errorf("unexpected files in %s: %v", linkDir, names)
	}
	e.assertNoLeftovers()
}

func TestUpdateNeverDowngradesOrReinstalls(t *testing.T) {
	for _, current := range []string{"1.1.0", "1.1.1", "1.2.0", "2.0.0", "10.0.0"} {
		t.Run(current, func(t *testing.T) {
			e := newEnv(t)
			e.opts.Version = current
			e.serveRelease("1.1.0")

			res, err := e.update()
			if err != nil {
				t.Fatal(err)
			}
			if res != (Result{Current: current, Latest: "1.1.0"}) {
				t.Errorf("result = %+v", res)
			}
			e.assertIntact()
			for _, r := range e.fx.requests() {
				if !strings.HasPrefix(r, "api.github.com/") {
					t.Errorf("downloaded something though nothing is newer: %s", r)
				}
			}
		})
	}
}

func TestUpdateRefusesDevelopmentBuilds(t *testing.T) {
	for _, ver := range []string{
		"dev", "", "(devel)", "1.2", "1.2.3.4", "v1.2.3", "01.2.3", "1.2.3-rc1",
		" 1.2.3", "1.2.3\n", "99999999999999999999.0.0",
	} {
		t.Run(ver, func(t *testing.T) {
			e := newEnv(t)
			e.opts.Version = ver
			e.serveRelease("9.9.9")

			_, err := e.update()
			if !errors.Is(err, ErrDevelopmentBuild) {
				t.Fatalf("err = %v, want ErrDevelopmentBuild", err)
			}
			if !strings.Contains(err.Error(), "development build") {
				t.Errorf("err = %q does not say what is wrong", err)
			}
			if got := e.fx.requests(); len(got) != 0 {
				t.Errorf("made requests: %v", got)
			}
			e.assertIntact()
		})
	}
}

func TestUpdateRefusesUnsupportedPlatforms(t *testing.T) {
	for _, p := range []struct{ goos, goarch string }{
		{"windows", "amd64"}, {"linux", "386"}, {"freebsd", "amd64"}, {"darwin", "riscv64"}, {"linux", "arm"}, {"", ""},
	} {
		t.Run(p.goos+"-"+p.goarch, func(t *testing.T) {
			e := newEnv(t)
			e.opts.GOOS, e.opts.GOARCH = p.goos, p.goarch
			e.serveRelease("1.1.0")

			_, err := e.update()
			if !errors.Is(err, ErrUnsupportedPlatform) {
				t.Fatalf("err = %v, want ErrUnsupportedPlatform", err)
			}
			if got := e.fx.requests(); len(got) != 0 {
				t.Errorf("made requests: %v", got)
			}
			e.assertIntact()
		})
	}
}

// Whatever goes wrong, the installed binary stays as it was and nothing is left
// in its directory or in TMPDIR.
func TestUpdateFailuresLeaveBinaryIntact(t *testing.T) {
	const ver = "1.1.0"
	file := archiveFile(ver, "linux", "amd64")
	sums := downloadRoute(ver, "SHA256SUMS")
	asset := downloadRoute(ver, file)
	cdn := cdnHost + "/asset/" + file

	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", code) }
	}
	body := func(s string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, s) }
	}
	// declare promises n bytes and sends none.
	declare := func(n int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(n))
			w.WriteHeader(http.StatusOK)
		}
	}
	// cutShort promises far more than it sends.
	cutShort := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "only a few bytes")
	}
	redirect := func(to string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }
	}

	good := fakeBinary("linux", "amd64", randomPayload(8192))
	goodArchive := makeArchive(t, binaryEntry(good))

	cases := []struct {
		name  string
		setup func(e *env)
		want  string // part of the error
	}{
		{"list unavailable", func(e *env) { e.fx.handle(apiRoute, status(500)) }, "list releases: HTTP 500"},
		{"list rate limited", func(e *env) { e.fx.handle(apiRoute, status(403)) }, "rate limited"},
		{"list is not JSON", func(e *env) { e.fx.handle(apiRoute, body("<html>nope</html>")) }, "unexpected response"},
		{"list is an object", func(e *env) { e.fx.handle(apiRoute, body(`{"message":"Not Found"}`)) }, "unexpected response"},
		{"list is null", func(e *env) { e.fx.handle(apiRoute, body("null")) }, "unexpected response"},
		{"list never ends", func(e *env) { e.fx.handle(apiRoute, body(page(stable("munch-v0.0.1")))) }, "did not end within 30 pages"},
		{"list too large", func(e *env) { e.fx.handle(apiRoute, declare(maxAPIBytes+1)) }, "exceeds"},
		{"no stable release", func(e *env) {
			e.fx.listPages(page(draft("munch-v2.0.0"), prerelease("munch-v3.0.0"), stable("v0.2.0")))
		}, "no stable munch release found"},

		{"sums missing", func(e *env) { e.fx.handle(sums, status(404)) }, "SHA256SUMS: HTTP 404"},
		{"sums too large", func(e *env) { e.fx.handle(sums, declare(maxSumsBytes+1)) }, "exceeds"},
		{"sums lack the archive", func(e *env) {
			e.fx.handle(sums, body(sumLine("munch-1.1.0-linux-arm64.tar.gz", goodArchive)))
		}, "0 entries"},
		{"sums list the archive twice", func(e *env) {
			e.fx.handle(sums, body(sumLine(file, goodArchive)+sumLine(file, goodArchive)))
		}, "2 entries"},
		{"sums list another digest", func(e *env) {
			e.fx.handle(sums, body(sumLine(file, []byte("something else"))))
		}, "checksum mismatch"},

		{"archive missing", func(e *env) { e.fx.handle(asset, status(404)) }, file + ": HTTP 404"},
		{"archive missing on the CDN", func(e *env) { e.fx.handle(cdn, status(404)) }, file + ": HTTP 404"},
		{"archive too large", func(e *env) { e.fx.handle(cdn, declare(maxArchiveBytes+1)) }, "exceeds"},
		{"archive cut short", func(e *env) { e.fx.handle(cdn, cutShort) }, "unexpected EOF"},
		{"archive redirects to plain http", func(e *env) {
			e.fx.handle(asset, redirect("http://evil.example/munch.tar.gz"))
		}, "non-HTTPS"},
		{"archive redirects forever", func(e *env) { e.fx.handle(asset, redirect("https://"+asset)) }, "stopped after 5 redirects"},
		{"no temporary directory", func(e *env) { e.t.Setenv("TMPDIR", filepath.Join(e.tmp, "missing")) }, "cannot create a temporary file"},

		{"archive is not gzip", func(e *env) { e.serveArchive(ver, []byte("plain text")) }, "not a gzip archive"},
		{"archive has no munch", func(e *env) {
			e.serveArchive(ver, makeArchive(e.t, tarEntry{name: "README", typeflag: tar.TypeReg, body: []byte("hi")}))
		}, "has no munch entry"},
		{"munch is a symlink", func(e *env) {
			e.serveArchive(ver, makeArchive(e.t, tarEntry{name: "munch", typeflag: tar.TypeSymlink, link: "/bin/sh"}))
		}, "not a regular file"},
		{"munch is a script", func(e *env) {
			e.serveArchive(ver, makeArchive(e.t, binaryEntry([]byte("#!/bin/sh\necho hi\n"))))
		}, "not an ELF executable"},
		{"munch is for another architecture", func(e *env) {
			e.serveArchive(ver, makeArchive(e.t, binaryEntry(fakeBinary("linux", "arm64", "x"))))
		}, "built for ELF machine 183"},
		{"archive is truncated", func(e *env) { e.serveArchive(ver, goodArchive[:len(goodArchive)/2]) }, "unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.serveArchive(ver, goodArchive)
			tc.setup(e)

			_, err := e.update()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			e.assertIntact()
		})
	}
}

func TestUpdateChecksVerdictBeforeReadingTheArchive(t *testing.T) {
	// An archive that is not even gzip, listed with the wrong digest, is
	// rejected on its digest: nothing was parsed.
	e := newEnv(t)
	e.serveArchive("1.1.0", []byte("not an archive"))
	e.fx.handle(downloadRoute("1.1.0", "SHA256SUMS"), func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sumLine(archiveFile("1.1.0", "linux", "amd64"), []byte("the archive that was signed")))
	})

	_, err := e.update()
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || strings.Contains(err.Error(), "gzip") {
		t.Fatalf("err = %v, want a checksum mismatch and no parsing", err)
	}
	e.assertIntact()
}

func TestUpdateKeepsABinaryReplacedDuringTheDownload(t *testing.T) {
	e := newEnv(t)
	good := fakeBinary("linux", "amd64", "munch 1.1.0")
	archive := makeArchive(t, binaryEntry(good))
	e.serveArchive("1.1.0", archive)
	other := filepath.Join(e.dir, "installed-by-someone-else")
	e.fx.handle(cdnHost+"/asset/"+archiveFile("1.1.0", "linux", "amd64"), func(w http.ResponseWriter, r *http.Request) {
		// Another update, or the installer script, lands first.
		if err := os.WriteFile(other, []byte("munch 1.2.0, installed meanwhile"), 0o755); err != nil {
			t.Errorf("fixture: %v", err)
		}
		if err := os.Rename(other, e.exe); err != nil {
			t.Errorf("fixture: %v", err)
		}
		w.Write(archive)
	})

	_, err := e.update()
	if err == nil || !strings.Contains(err.Error(), "replaced while the update was running") {
		t.Fatalf("err = %v, want it to say the binary was replaced meanwhile", err)
	}
	if got, err := os.ReadFile(e.exe); err != nil || string(got) != "munch 1.2.0, installed meanwhile" {
		t.Errorf("binary = %q (err=%v), want the one installed meanwhile", got, err)
	}
	e.assertNoLeftovers()
}

func TestUpdateDoesNotRecreateABinaryRemovedDuringTheDownload(t *testing.T) {
	e := newEnv(t)
	archive := makeArchive(t, binaryEntry(fakeBinary("linux", "amd64", "munch 1.1.0")))
	e.serveArchive("1.1.0", archive)
	e.fx.handle(cdnHost+"/asset/"+archiveFile("1.1.0", "linux", "amd64"), func(w http.ResponseWriter, r *http.Request) {
		if err := os.Remove(e.exe); err != nil {
			t.Errorf("fixture: %v", err)
		}
		w.Write(archive)
	})

	_, err := e.update()
	if err == nil || !strings.Contains(err.Error(), "before replacing it") {
		t.Fatalf("err = %v, want it to say the binary could not be inspected", err)
	}
	if names := dirNames(t, e.dir); len(names) != 0 {
		t.Errorf("files left in %s: %v", e.dir, names)
	}
	if names := dirNames(t, e.tmp); len(names) != 0 {
		t.Errorf("temporary files left in TMPDIR: %v", names)
	}
}

func TestUpdateNeedsAWritableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not restrict this user")
	}
	e := newEnv(t)
	e.serveRelease("1.1.0")
	if err := os.Chmod(e.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.dir, 0o755) })

	_, err := e.update()
	if err == nil || !strings.Contains(err.Error(), "write access") {
		t.Fatalf("err = %v, want it to ask for write access", err)
	}
	for _, r := range e.fx.requests() {
		if !strings.HasPrefix(r, "api.github.com/") {
			t.Errorf("downloaded before checking the directory: %s", r)
		}
	}
	e.assertIntact()
}

func TestUpdateStopsWhenTheContextIsCanceled(t *testing.T) {
	e := newEnv(t)
	e.serveRelease("1.1.0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Update(ctx, e.opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	e.assertIntact()
}

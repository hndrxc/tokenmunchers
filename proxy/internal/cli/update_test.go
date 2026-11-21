package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/selfupdate"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/service"
)

// updateFixture is a plain-HTTP stand-in for GitHub: the release list of
// api.github.com, and the release downloads of github.com, which redirect to a
// CDN host as the real ones do. As a RoundTripper it carries every request the
// updater makes to itself, so the updater runs unmodified against the official
// URLs.
type updateFixture struct {
	srv     *httptest.Server
	latest  string // newest stable release the list publishes
	archive []byte // munch-<latest>-linux-amd64.tar.gz
	sums    string // SHA256SUMS

	mu   sync.Mutex
	seen []string // host and URI of each request, in order
}

func newUpdateFixture(t *testing.T, latest string, archive []byte, sums string) *updateFixture {
	t.Helper()
	f := &updateFixture{latest: latest, archive: archive, sums: sums}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *updateFixture) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *updateFixture) forgetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = nil
}

// RoundTrip sends the request to the fixture server over plain HTTP, keeping
// the original host so the server can route on it. Anything but HTTPS is
// refused: the updater must never ask for it.
func (f *updateFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("fixture: refusing a %s request to %s", req.URL.Scheme, req.URL.Host)
	}
	out := req.Clone(req.Context())
	out.Host = req.URL.Host
	out.URL.Scheme = "http"
	out.URL.Host = f.srv.Listener.Addr().String()
	return f.srv.Client().Transport.RoundTrip(out)
}

func (f *updateFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Host+r.URL.RequestURI())
	f.mu.Unlock()

	release := "github.com/hndrxc/tokenmunchers/releases/download/munch-v" + f.latest + "/"
	switch r.Host + r.URL.Path {
	case "api.github.com/repos/hndrxc/tokenmunchers/releases":
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.github+json") ||
			r.Header.Get("X-GitHub-Api-Version") == "" ||
			!strings.HasPrefix(r.Header.Get("User-Agent"), "munch-update/") {
			http.Error(w, "missing GitHub API headers", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, f.releasePage(r.URL.Query().Get("page")))
	case release + "SHA256SUMS":
		fmt.Fprint(w, f.sums)
	case release + "munch-" + f.latest + "-linux-amd64.tar.gz":
		http.Redirect(w, r, "https://release-assets.githubusercontent.com/archive", http.StatusFound)
	case "release-assets.githubusercontent.com/archive":
		w.Write(f.archive)
	default:
		http.NotFound(w, r)
	}
}

// releasePage lists releases newest first, as GitHub does: drafts, a
// prerelease, another project's tag, and an older stable release come before
// the latest stable one, which sits on the second page.
func (f *updateFixture) releasePage(page string) string {
	switch page {
	case "1":
		return `[{"tag_name":"munch-v9.0.0","draft":true},` +
			`{"tag_name":"munch-v8.0.0","prerelease":true},` +
			`{"tag_name":"v0.2.0"},` +
			`{"tag_name":"munch-v1.3.0"}]`
	case "2":
		return `[{"tag_name":"munch-v` + f.latest + `"},{"tag_name":"munch-v1.2.0"}]`
	}
	return `[]`
}

// updateTestBinary is a fake linux/amd64 executable: a valid ELF header, which
// is all the updater inspects, followed by marker.
func updateTestBinary(marker string) []byte {
	head := make([]byte, 64)
	copy(head, "\x7fELF")
	head[4], head[5], head[6] = 2, 1, 1 // 64-bit, little-endian, version 1
	head[16] = 2                        // ET_EXEC
	head[18] = 62                       // EM_X86_64
	return append(head, marker...)
}

// updateTestArchive packs binary as the single munch entry of a gzipped tar.
func updateTestArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "munch", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binary))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// updateTestSums is a SHA256SUMS listing archive as munch-<version>-linux-amd64.
func updateTestSums(version string, archive []byte) string {
	other := sha256.Sum256([]byte("an archive for another platform"))
	mine := sha256.Sum256(archive)
	return fmt.Sprintf("%x  munch-%s-darwin-arm64.tar.gz\n%x  munch-%s-linux-amd64.tar.gz\n", other, version, mine, version)
}

// updateTestDir returns a fresh directory with symbolic links resolved, so
// paths compare equal to the ones the updater reports.
func updateTestDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func updateTestWriteFile(t *testing.T, path string, content []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // WriteFile applies the umask
		t.Fatal(err)
	}
}

// updateTestIsolate points the temporary directory and munch's config and state
// directories at scratch locations, so a test sees anything the update leaves
// in the first or creates in the others.
func updateTestIsolate(t *testing.T) (tmpDir, cfgDir, stateDir string) {
	t.Helper()
	tmpDir = updateTestDir(t)
	root := updateTestDir(t)
	cfgDir, stateDir = filepath.Join(root, "cfg"), filepath.Join(root, "state")
	t.Setenv("TMPDIR", tmpDir)
	t.Setenv("TMPROXY_CONFIG_DIR", cfgDir)
	t.Setenv("TMPROXY_STATE_DIR", stateDir)
	return tmpDir, cfgDir, stateDir
}

func updateTestNames(t *testing.T, dir string) []string {
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

// useUpdateFixture makes `munch update` behave as a build of the given version
// whose executable is exe, served by fx.
func useUpdateFixture(t *testing.T, fx *updateFixture, version, exe string) {
	t.Helper()
	orig := selfUpdateOptions
	t.Cleanup(func() { selfUpdateOptions = orig })
	selfUpdateOptions = func() (selfupdate.Options, error) {
		return selfupdate.Options{
			Version:    version,
			GOOS:       "linux",
			GOARCH:     "amd64",
			Executable: exe,
			Transport:  fx,
		}, nil
	}
}

// runUpdateCommand runs `munch update args...` and fails the test if it
// touches the background service.
func runUpdateCommand(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	app := &App{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errBuf,
		newService: func(config.Paths, string) service.Manager {
			t.Error("munch update must not touch the background service")
			return nil
		},
	}
	code = app.Run(append([]string{"update"}, args...))
	return code, out.String(), errBuf.String()
}

var updateListRequests = []string{
	"api.github.com/repos/hndrxc/tokenmunchers/releases?per_page=100&page=1",
	"api.github.com/repos/hndrxc/tokenmunchers/releases?per_page=100&page=2",
	"api.github.com/repos/hndrxc/tokenmunchers/releases?per_page=100&page=3",
}

func TestUpdateCommandInstallsLatestStableRelease(t *testing.T) {
	realDir, binDir := updateTestDir(t), updateTestDir(t)
	realBin := filepath.Join(realDir, "munch")
	link := filepath.Join(binDir, "munch")
	updateTestWriteFile(t, realBin, []byte("munch 1.3.0"), 0o750)
	if err := os.Symlink(realBin, link); err != nil {
		t.Fatal(err)
	}

	newBinary := updateTestBinary("munch 1.4.0")
	archive := updateTestArchive(t, newBinary)
	fx := newUpdateFixture(t, "1.4.0", archive, updateTestSums("1.4.0", archive))
	useUpdateFixture(t, fx, "1.3.0", link)
	tmpDir, cfgDir, stateDir := updateTestIsolate(t)

	code, out, errOut := runUpdateCommand(t)
	if code != 0 || errOut != "" {
		t.Fatalf("update failed (code=%d): out=%s err=%s", code, out, errOut)
	}

	// The file the symlink leads to was replaced, keeping its permissions; the
	// symlink itself was not.
	got, err := os.ReadFile(realBin)
	if err != nil || !bytes.Equal(got, newBinary) {
		t.Fatalf("binary behind the symlink was not replaced: err=%v", err)
	}
	if info, err := os.Stat(realBin); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("permissions not preserved: info=%v err=%v", info, err)
	}
	if dest, err := os.Readlink(link); err != nil || dest != realBin {
		t.Fatalf("symlink changed: dest=%q err=%v", dest, err)
	}
	for _, dir := range []string{realDir, binDir} {
		if names := updateTestNames(t, dir); !reflect.DeepEqual(names, []string{"munch"}) {
			t.Fatalf("unexpected files left in %s: %v", dir, names)
		}
	}
	if names := updateTestNames(t, tmpDir); len(names) != 0 {
		t.Fatalf("temporary download left behind: %v", names)
	}

	// What the user sees, including how to activate the new binary.
	for _, want := range []string{"1.3.0", "1.4.0", realBin, "`munch restart`"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// Only the official URLs were used, in order: the whole release list, then
	// SHA256SUMS, then the archive and its CDN redirect.
	wantRequests := append(append([]string(nil), updateListRequests...),
		"github.com/hndrxc/tokenmunchers/releases/download/munch-v1.4.0/SHA256SUMS",
		"github.com/hndrxc/tokenmunchers/releases/download/munch-v1.4.0/munch-1.4.0-linux-amd64.tar.gz",
		"release-assets.githubusercontent.com/archive",
	)
	if got := fx.requests(); !reflect.DeepEqual(got, wantRequests) {
		t.Errorf("requests:\n got %v\nwant %v", got, wantRequests)
	}

	// Configuration, credentials and state were not created or touched.
	for _, dir := range []string{cfgDir, stateDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("update touched %s (stat err=%v)", dir, err)
		}
	}

	// Updating again finds nothing newer and downloads nothing.
	useUpdateFixture(t, fx, "1.4.0", link)
	fx.forgetRequests()
	code, out, errOut = runUpdateCommand(t)
	if code != 0 || errOut != "" || !strings.Contains(out, "already the latest") {
		t.Fatalf("second update (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got := fx.requests(); !reflect.DeepEqual(got, updateListRequests) {
		t.Errorf("second update requests:\n got %v\nwant %v", got, updateListRequests)
	}
}

func TestUpdateCommandNeverDowngrades(t *testing.T) {
	exe := filepath.Join(updateTestDir(t), "munch")
	updateTestWriteFile(t, exe, []byte("munch 2.0.0 from source"), 0o755)
	archive := updateTestArchive(t, updateTestBinary("munch 1.4.0"))
	fx := newUpdateFixture(t, "1.4.0", archive, updateTestSums("1.4.0", archive))
	// Newer than every stable release: the 9.0.0 draft and 8.0.0 prerelease
	// in the list do not count.
	useUpdateFixture(t, fx, "2.0.0", exe)
	updateTestIsolate(t)

	code, out, errOut := runUpdateCommand(t)
	if code != 0 || errOut != "" || !strings.Contains(out, "newer than the latest published release (1.4.0)") {
		t.Fatalf("update (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "munch 2.0.0 from source" {
		t.Fatalf("binary was modified: %q err=%v", got, err)
	}
	if got := fx.requests(); !reflect.DeepEqual(got, updateListRequests) {
		t.Errorf("requests:\n got %v\nwant %v", got, updateListRequests)
	}
}

func TestUpdateCommandFailureLeavesBinaryIntact(t *testing.T) {
	dir := updateTestDir(t)
	exe := filepath.Join(dir, "munch")
	updateTestWriteFile(t, exe, []byte("munch 1.3.0"), 0o755)
	archive := updateTestArchive(t, updateTestBinary("munch 1.4.0"))
	// SHA256SUMS lists the digest of some other file for this archive.
	fx := newUpdateFixture(t, "1.4.0", archive, updateTestSums("1.4.0", []byte("not the archive")))
	useUpdateFixture(t, fx, "1.3.0", exe)
	tmpDir, _, _ := updateTestIsolate(t)

	code, out, errOut := runUpdateCommand(t)
	if code != 1 || !strings.Contains(errOut, "checksum mismatch") || strings.Contains(out, "Updated") {
		t.Fatalf("update (code=%d): out=%s err=%s", code, out, errOut)
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "munch 1.3.0" {
		t.Fatalf("binary was modified: %q err=%v", got, err)
	}
	if names := updateTestNames(t, dir); !reflect.DeepEqual(names, []string{"munch"}) {
		t.Fatalf("unexpected files left in %s: %v", dir, names)
	}
	if names := updateTestNames(t, tmpDir); len(names) != 0 {
		t.Fatalf("temporary download left behind: %v", names)
	}
}

func TestUpdateCommandRefusesDevelopmentBuild(t *testing.T) {
	// The real options, not a fixture: a source build must refuse before it
	// resolves, let alone contacts, anything.
	orig := config.Version
	config.Version = "dev"
	t.Cleanup(func() { config.Version = orig })

	code, out, errOut := runUpdateCommand(t)
	if code != 1 || out != "" || !strings.Contains(errOut, "development build") {
		t.Fatalf("update (code=%d): out=%s err=%s", code, out, errOut)
	}
}

func TestUpdateCommandArguments(t *testing.T) {
	if code, out, errOut := runUpdateCommand(t, "extra"); code != 1 || out != "" || !strings.Contains(errOut, "takes no arguments") {
		t.Errorf("update extra (code=%d): out=%s err=%s", code, out, errOut)
	}
	if code, out, errOut := runUpdateCommand(t, "--bogus"); code != 1 || out != "" || !strings.Contains(errOut, "bogus") {
		t.Errorf("update --bogus (code=%d): out=%s err=%s", code, out, errOut)
	}
	for _, flagName := range []string{"-h", "--help"} {
		code, out, errOut := runUpdateCommand(t, flagName)
		if code != 0 || out != "" || !strings.Contains(errOut, "Usage: munch update") || strings.Contains(errOut, "error:") {
			t.Errorf("update %s (code=%d): out=%s err=%s", flagName, code, out, errOut)
		}
	}
}

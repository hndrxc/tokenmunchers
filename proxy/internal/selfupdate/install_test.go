package selfupdate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	valid := map[string]version{
		"0.0.0":                    {0, 0, 0},
		"1.2.3":                    {1, 2, 3},
		"10.20.30":                 {10, 20, 30},
		"18446744073709551615.0.0": {18446744073709551615, 0, 0},
	}
	for s, want := range valid {
		if got, ok := parseVersion(s); !ok || got != want {
			t.Errorf("parseVersion(%q) = %v, %v; want %v", s, got, ok, want)
		}
	}
	for _, s := range []string{
		"", "dev", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "-1.2.3", "+1.2.3",
		"1.2.3-rc1", "v1.2.3", " 1.2.3", "1.2.3 ", "1..3", "a.b.c", "18446744073709551616.0.0",
	} {
		if got, ok := parseVersion(s); ok {
			t.Errorf("parseVersion(%q) = %v, want it rejected", s, got)
		}
	}
}

func TestParseTag(t *testing.T) {
	if got, ok := parseTag("munch-v1.2.3"); !ok || got != (version{1, 2, 3}) {
		t.Errorf("parseTag(munch-v1.2.3) = %v, %v", got, ok)
	}
	for _, tag := range []string{
		"", "v1.2.3", "munch-1.2.3", "xmunch-v1.2.3", "Munch-v1.2.3", "munch-v", "munch-v1.2", "munch-v1.2.3-rc1",
	} {
		if got, ok := parseTag(tag); ok {
			t.Errorf("parseTag(%q) = %v, want it rejected", tag, got)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.9.9", "1.10.0", -1}, // numbers, not text
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "1.99.99", 1},
		{"0.1.1", "0.1.0", 1},
		{"0.0.9", "0.1.0", -1},
	} {
		a, _ := parseVersion(c.a)
		b, _ := parseVersion(c.b)
		if got := a.compare(b); got != c.want {
			t.Errorf("%s compare %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestDigestFor(t *testing.T) {
	const file = "munch-1.1.0-linux-amd64.tar.gz"
	hash := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	cases := []struct {
		name string
		sums string
		want string // the digest; empty if an error is expected
		err  string // part of that error
	}{
		{"one entry", hash + "  " + file + "\n", hash, ""},
		{"among others", other + "  munch-1.1.0-linux-arm64.tar.gz\n" + hash + "  " + file + "\n" + other + "  munch-1.1.0-darwin-amd64.tar.gz\n", hash, ""},
		{"no trailing newline", hash + "  " + file, hash, ""},
		{"windows line endings", hash + "  " + file + "\r\n", hash, ""},

		{"empty", "", "", "0 entries"},
		{"other archives only", other + "  munch-1.1.0-linux-arm64.tar.gz\n", "", "0 entries"},
		{"listed name only starts like ours", hash + "  " + file + ".sig\n", "", "0 entries"},
		{"listed with a path", hash + "  ./" + file + "\n", "", "0 entries"},
		{"listed in binary mode", hash + " *" + file + "\n", "", "0 entries"},
		{"listed twice", hash + "  " + file + "\n" + hash + "  " + file + "\n", "", "2 entries"},
		{"listed twice with different digests", hash + "  " + file + "\n" + other + "  " + file + "\n", "", "2 entries"},
		{"uppercase digest", strings.ToUpper(hash) + "  " + file + "\n", "", "invalid digest"},
		{"short digest", hash[:63] + "  " + file + "\n", "", "invalid digest"},
		{"long digest", hash + "0  " + file + "\n", "", "invalid digest"},
		{"non-hex digest", hash[:63] + "g  " + file + "\n", "", "invalid digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := digestFor([]byte(tc.sums), file)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("digestFor = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestExtractBinary(t *testing.T) {
	good := fakeBinary("linux", "amd64", "munch 1.1.0")
	big := fakeBinary("linux", "amd64", randomPayload(8192))
	bigArchive := makeArchive(t, binaryEntry(big))

	cases := []struct {
		name    string
		archive []byte
		out     []byte // the extracted binary, if the archive is acceptable
		want    string // part of the error otherwise
	}{
		{"regular file", makeArchive(t, binaryEntry(good)), good, ""},
		{"other entries are ignored", makeArchive(t, tarEntry{name: "LICENSE", typeflag: tar.TypeReg, body: []byte("MIT")}, binaryEntry(good)), good, ""},
		{"large binary", bigArchive, big, ""},

		{"not gzip", []byte("plain text"), nil, "not a gzip archive"},
		{"empty input", nil, nil, "not a gzip archive"},
		{"gzip of something else", gzipOf(t, bytes.Repeat([]byte("x"), 600)), nil, "read archive"},
		{"no entries", makeArchive(t), nil, "has no munch entry"},
		{"no munch entry", makeArchive(t, tarEntry{name: "README", typeflag: tar.TypeReg, body: []byte("hi")}), nil, "has no munch entry"},
		{"munch in a subdirectory", makeArchive(t, tarEntry{name: "bin/munch", typeflag: tar.TypeReg, body: good}), nil, "has no munch entry"},
		{"munch with a dot prefix", makeArchive(t, tarEntry{name: "./munch", typeflag: tar.TypeReg, body: good}), nil, "has no munch entry"},
		{"munch is a symlink", makeArchive(t, tarEntry{name: "munch", typeflag: tar.TypeSymlink, link: "/bin/sh"}), nil, "not a regular file"},
		{"munch is a hard link", makeArchive(t, tarEntry{name: "munch", typeflag: tar.TypeLink, link: "other"}), nil, "not a regular file"},
		{"munch is a directory", makeArchive(t, tarEntry{name: "munch", typeflag: tar.TypeDir}), nil, "not a regular file"},
		{"munch is empty", makeArchive(t, binaryEntry(nil)), nil, "is empty"},
		{"munch twice", makeArchive(t, binaryEntry(good), binaryEntry(good)), nil, "more than one munch entry"},
		{"munch is a script", makeArchive(t, binaryEntry([]byte("#!/bin/sh\necho hi\n"))), nil, "not an ELF executable"},
		{"munch is shorter than a header", makeArchive(t, binaryEntry(good[:10])), nil, "not an ELF executable"},
		{"munch is for another architecture", makeArchive(t, binaryEntry(fakeBinary("linux", "arm64", "x"))), nil, "built for ELF machine 183"},
		{"truncated archive", bigArchive[:len(bigArchive)/2], nil, "unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := extractBinary(&out, bytes.NewReader(tc.archive), "linux", "amd64")
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.want)
				}
				return
			}
			if err != nil || !bytes.Equal(out.Bytes(), tc.out) {
				t.Fatalf("extractBinary = %v, wrote %d bytes; want the %d byte binary", err, out.Len(), len(tc.out))
			}
		})
	}
}

func patched(b []byte, at int, v ...byte) []byte {
	out := append([]byte(nil), b...)
	copy(out[at:], v)
	return out
}

func TestCheckExecutable(t *testing.T) {
	elfAMD := fakeBinary("linux", "amd64", "")
	elfARM := fakeBinary("linux", "arm64", "")
	machoAMD := fakeBinary("darwin", "amd64", "")
	machoARM := fakeBinary("darwin", "arm64", "")

	cases := []struct {
		name, goos, goarch string
		head               []byte
		want               string // part of the error; empty if the header is acceptable
	}{
		{"linux amd64", "linux", "amd64", elfAMD, ""},
		{"linux arm64", "linux", "arm64", elfARM, ""},
		{"darwin amd64", "darwin", "amd64", machoAMD, ""},
		{"darwin arm64", "darwin", "arm64", machoARM, ""},
		{"position-independent ELF", "linux", "amd64", patched(elfAMD, 16, 3, 0), ""},

		{"empty", "linux", "amd64", nil, "not an ELF executable"},
		{"truncated ELF header", "linux", "amd64", elfAMD[:40], "not an ELF executable"},
		{"script", "linux", "amd64", []byte("#!/bin/sh\necho hi\n"), "not an ELF executable"},
		{"32-bit ELF", "linux", "amd64", patched(elfAMD, 4, 1), "64-bit little-endian ELF"},
		{"big-endian ELF", "linux", "amd64", patched(elfAMD, 5, 2), "64-bit little-endian ELF"},
		{"ELF object file", "linux", "amd64", patched(elfAMD, 16, 1, 0), "not an executable"},
		{"x86-64 ELF on arm64", "linux", "arm64", elfAMD, "built for ELF machine 62"},
		{"arm64 ELF on amd64", "linux", "amd64", elfARM, "built for ELF machine 183"},
		{"Mach-O on linux", "linux", "amd64", machoAMD, "not an ELF executable"},

		{"ELF on darwin", "darwin", "arm64", elfARM, "Mach-O"},
		{"truncated Mach-O header", "darwin", "arm64", machoARM[:20], "Mach-O"},
		{"Mach-O dylib", "darwin", "arm64", patched(machoARM, 12, 6, 0, 0, 0), "not an executable"},
		{"x86-64 Mach-O on arm64", "darwin", "arm64", machoAMD, "Mach-O CPU 0x1000007"},
		{"arm64 Mach-O on amd64", "darwin", "amd64", machoARM, "Mach-O CPU 0x100000c"},

		{"unsupported system", "plan9", "amd64", elfAMD, "supports linux and darwin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkExecutable(tc.head, tc.goos, tc.goarch)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if err := checkExecutable(elfAMD, "plan9", "amd64"); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("unsupported system: err = %v, want ErrUnsupportedPlatform", err)
	}
}

func TestCapReader(t *testing.T) {
	got, err := io.ReadAll(&capReader{r: strings.NewReader("abcdef"), left: 6})
	if err != nil || string(got) != "abcdef" {
		t.Errorf("exactly at the cap: %q, %v", got, err)
	}
	if _, err := io.ReadAll(&capReader{r: strings.NewReader("abcdefg"), left: 6}); !errors.Is(err, errExtractedTooLarge) {
		t.Errorf("one byte over the cap: err = %v, want errExtractedTooLarge", err)
	}
	// Small reads cannot sneak past the cap either.
	r := &capReader{r: strings.NewReader("abcdefghij"), left: 6}
	one := make([]byte, 1)
	var err2 error
	for i := 0; i < 10 && err2 == nil; i++ {
		_, err2 = r.Read(one)
	}
	if !errors.Is(err2, errExtractedTooLarge) {
		t.Errorf("one byte at a time: err = %v, want errExtractedTooLarge", err2)
	}
}

func TestReadLimited(t *testing.T) {
	for _, size := range []int64{-1, 0, 2, 5, 1 << 40} { // a wrong hint only changes the allocation
		if got, err := readLimited(strings.NewReader("12345"), 5, size); err != nil || string(got) != "12345" {
			t.Errorf("at the limit, size hint %d: %q, %v", size, got, err)
		}
	}
	if _, err := readLimited(strings.NewReader("123456"), 5, -1); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("over the limit: err = %v", err)
	}
}

func TestRequireHTTPS(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	if err := requireHTTPS(req("https://objects.example/x"), make([]*http.Request, 1)); err != nil {
		t.Errorf("https redirect refused: %v", err)
	}
	if err := requireHTTPS(req("http://objects.example/x"), make([]*http.Request, 1)); err == nil || !strings.Contains(err.Error(), "non-HTTPS") {
		t.Errorf("plain http redirect: err = %v, want it refused", err)
	}
	if err := requireHTTPS(req("https://objects.example/x"), make([]*http.Request, maxRedirects)); err != nil {
		t.Errorf("redirect number %d refused: %v", maxRedirects, err)
	}
	if err := requireHTTPS(req("https://objects.example/x"), make([]*http.Request, maxRedirects+1)); err == nil {
		t.Errorf("redirect number %d allowed", maxRedirects+1)
	}
}

func testFetcher(rt http.RoundTripper) *fetcher {
	return &fetcher{
		client:    &http.Client{Transport: rt, CheckRedirect: requireHTTPS},
		userAgent: "munch-update/test",
	}
}

func TestFetcherGetBoundsResponses(t *testing.T) {
	fx := newFixture(t)
	fx.handle("github.com/exact", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 1024))
	})
	fx.handle("github.com/over", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 1025))
	})
	fx.handle("github.com/streamed", func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length: the size is only known by reading.
		for range 3 {
			io.WriteString(w, strings.Repeat("x", 600))
			w.(http.Flusher).Flush()
		}
	})
	fx.handle("github.com/declared", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
	})
	f := testFetcher(fx)
	ctx := context.Background()

	if got, err := f.get(ctx, "https://github.com/exact", "exact", time.Minute, 1024, nil); err != nil || len(got) != 1024 {
		t.Errorf("exactly at the limit: %d bytes, %v", len(got), err)
	}
	for _, path := range []string{"over", "streamed", "declared"} {
		_, err := f.get(ctx, "https://github.com/"+path, path, time.Minute, 1024, nil)
		if err == nil || !strings.Contains(err.Error(), "exceeds the 1024 byte limit") {
			t.Errorf("%s: err = %v, want a size limit error", path, err)
		}
	}
}

func TestFetcherDownloadHashesAndBoundsTheBody(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 500) // 5000 bytes
	fx := newFixture(t)
	fx.handle("github.com/blob", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	fx.handle("github.com/streamed", func(w http.ResponseWriter, r *http.Request) {
		for range 10 {
			w.Write(payload)
			w.(http.Flusher).Flush()
		}
	})
	f := testFetcher(fx)
	ctx := context.Background()

	var dst bytes.Buffer
	digest, err := f.download(ctx, "https://github.com/blob", "blob", time.Minute, 5000, nil, &dst)
	if want := fmt.Sprintf("%x", sha256.Sum256(payload)); err != nil || digest != want || !bytes.Equal(dst.Bytes(), payload) {
		t.Fatalf("download = %q, %v (wrote %d bytes); want digest %q of the payload", digest, err, dst.Len(), want)
	}
	for _, path := range []string{"blob", "streamed"} {
		_, err := f.download(ctx, "https://github.com/"+path, path, time.Minute, 4999, nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "exceeds the 4999 byte limit") {
			t.Errorf("%s: err = %v, want a size limit error", path, err)
		}
	}
}

func TestFetcherReportsHTTPStatus(t *testing.T) {
	fx := newFixture(t)
	f := testFetcher(fx)
	for _, tc := range []struct {
		code int
		want string
	}{
		{http.StatusNotFound, "thing: HTTP 404"},
		{http.StatusForbidden, "thing: HTTP 403 (rate limited? try again later)"},
		{http.StatusTooManyRequests, "thing: HTTP 429 (rate limited? try again later)"},
		{http.StatusInternalServerError, "thing: HTTP 500"},
		{http.StatusNoContent, "thing: HTTP 204"}, // only a 200 is a download
	} {
		fx.handle("github.com/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code) })
		_, err := f.get(context.Background(), "https://github.com/status", "thing", time.Minute, 1024, nil)
		if err == nil || err.Error() != tc.want {
			t.Errorf("status %d: err = %v, want %q", tc.code, err, tc.want)
		}
	}
}

func TestFetcherStopsAtTheTimeout(t *testing.T) {
	fx := newFixture(t)
	stall := func(r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	fx.handle("github.com/no-response", func(w http.ResponseWriter, r *http.Request) { stall(r) })
	fx.handle("github.com/stalled-body", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		stall(r)
	})
	f := testFetcher(fx)

	for _, path := range []string{"no-response", "stalled-body"} {
		rawURL := "https://github.com/" + path
		start := time.Now()
		_, err := f.get(context.Background(), rawURL, path, 100*time.Millisecond, 1<<20, nil)
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Errorf("get %s: err = %v, want a deadline error", path, err)
		}
		_, err = f.download(context.Background(), rawURL, path, 100*time.Millisecond, 1<<20, nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Errorf("download %s: err = %v, want a deadline error", path, err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("%s: took %v, the timeout did not bound it", path, elapsed)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetcherErrorsDoNotRevealRedirectTargets(t *testing.T) {
	calls := 0
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": {"https://cdn.example/asset?signature=SECRET"}},
				Body:       http.NoBody,
				Request:    r,
			}, nil
		}
		return nil, errors.New("connection reset")
	}))

	_, err := f.get(context.Background(), "https://github.com/asset", "asset", time.Minute, 1024, nil)
	if err == nil || !strings.Contains(err.Error(), "asset: connection reset") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v, want the cause without the signed address", err)
	}
}

func TestResolveTarget(t *testing.T) {
	dir := realTempDir(t)
	file := filepath.Join(dir, "munch")
	writeFile(t, file, []byte("x"), 0o750)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{file, link} {
		got, err := resolveTarget(path)
		if err != nil || got.path != file || got.info.Mode().Perm() != 0o750 {
			t.Errorf("resolveTarget(%s) = %+v, %v; want %s with mode 750", path, got, err, file)
		}
	}
	for name, path := range map[string]string{
		"empty path":    "",
		"missing file":  filepath.Join(dir, "missing"),
		"dangling link": dangling,
		"directory":     dir,
	} {
		if got, err := resolveTarget(path); err == nil {
			t.Errorf("%s: resolveTarget(%q) = %+v, want an error", name, path, got)
		}
	}
	if _, err := resolveTarget(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: err = %v, want it to say it is not a regular file", err)
	}
}

func TestNewScratchLeavesNothingBehind(t *testing.T) {
	tmp := realTempDir(t)
	t.Setenv("TMPDIR", tmp)

	f, err := newScratch()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if names := dirNames(t, tmp); len(names) != 0 {
		t.Fatalf("scratch file is visible in TMPDIR: %v", names)
	}
	// Unlinked, it is still a file: it can be written, rewound and read back.
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(f); err != nil || string(got) != "hello" {
		t.Errorf("read back %q, %v", got, err)
	}
}

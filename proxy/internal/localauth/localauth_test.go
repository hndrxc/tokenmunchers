package localauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testKey() Key {
	var k Key
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestDerivationsAreDomainSeparated(t *testing.T) {
	k := testKey()
	nonce := strings.Repeat("ab", 32)

	route := k.RoutePrefix()
	if !strings.HasPrefix(route, "/_tm/") || strings.HasSuffix(route, "/") || len(route) != len("/_tm/")+64 {
		t.Fatalf("unexpected route prefix shape: %q", route)
	}
	capability := strings.TrimPrefix(route, "/_tm/")
	control := k.ControlToken()
	proof := k.Proof(nonce)

	if capability == control || capability == proof || control == proof {
		t.Fatal("derived credentials must differ")
	}
	for _, v := range []string{capability, control, proof} {
		if _, err := hex.DecodeString(v); err != nil || len(v) != 64 || v != strings.ToLower(v) {
			t.Fatalf("credential is not 64 lowercase hex chars")
		}
		if strings.Contains(v, hex.EncodeToString(k[:])) {
			t.Fatal("credential leaks raw key")
		}
	}

	want := func(domain, data string) string {
		h := hmac.New(sha256.New, k[:])
		h.Write([]byte(domain + "\x00" + data))
		return hex.EncodeToString(h.Sum(nil))
	}
	if capability != want("tokenmunchers/localauth/v1/route-capability", "") ||
		control != want("tokenmunchers/localauth/v1/control-token", "") ||
		proof != want("tokenmunchers/localauth/v1/server-proof", nonce) {
		t.Fatal("derivation does not match documented construction")
	}
	if k.Proof(strings.Repeat("cd", 32)) == proof {
		t.Fatal("proof must depend on nonce")
	}
	other := testKey()
	other[0] ^= 1
	if other.RoutePrefix() == route || other.ControlToken() == control || other.Proof(nonce) == proof {
		t.Fatal("credentials must depend on key")
	}
}

func TestInvalidInputsFailClosed(t *testing.T) {
	var zero Key
	if zero.RoutePrefix() != "" || zero.ControlToken() != "" || zero.Proof(strings.Repeat("a", 64)) != "" {
		t.Fatal("zero key must yield empty credentials")
	}
	k := testKey()
	for _, n := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63) + "\n"} {
		if k.Proof(n) != "" || ValidNonce(n) {
			t.Fatalf("nonce %q must be rejected", n)
		}
	}
	nonce, err := NewNonce()
	if err != nil || !ValidNonce(nonce) {
		t.Fatalf("NewNonce = %q, %v", nonce, err)
	}
	if !k.VerifyControlToken(k.ControlToken()) || k.VerifyControlToken("") || k.VerifyControlToken(k.ControlToken()+"0") {
		t.Fatal("control token verification wrong")
	}
	if !k.VerifyProof(nonce, k.Proof(nonce)) || k.VerifyProof(nonce, "") || k.VerifyProof("bad", k.Proof(nonce)) {
		t.Fatal("proof verification wrong")
	}
	if zero.VerifyControlToken("") || zero.VerifyProof(nonce, "") {
		t.Fatal("zero key must never verify")
	}
}

func TestKeyNeverFormatsOrMarshals(t *testing.T) {
	k := testKey()
	raw := hex.EncodeToString(k[:])
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d"} {
		if out := fmt.Sprintf(f, k); strings.Contains(out, raw) || strings.Contains(out, "1 2 3") {
			t.Fatalf("format %s leaked key: %q", f, out)
		}
	}
	if _, err := k.MarshalText(); err == nil {
		t.Fatal("MarshalText must fail")
	}
}

func TestLoadOrCreatePersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cfg")
	if _, err := Load(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of missing dir = %v, want ErrNotExist", err)
	}
	k1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if k1.IsZero() {
		t.Fatal("created zero key")
	}
	k2, err := LoadOrCreate(dir)
	if err != nil || k2 != k1 {
		t.Fatalf("second LoadOrCreate = %v, %v", k2 == k1, err)
	}
	k3, err := Load(dir)
	if err != nil || k3 != k1 {
		t.Fatalf("Load = %v, %v", k3 == k1, err)
	}
	assertNoTemp(t, dir)
}

func TestLoadMissingFileIsNotExist(t *testing.T) {
	if _, err := Load(privateDir(t)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load = %v, want ErrNotExist", err)
	}
}

func TestInvalidFileFailsClosedAndIsNotOverwritten(t *testing.T) {
	good := strings.Repeat("ab", 32)
	cases := map[string]string{
		"empty":     "",
		"garbage":   "not a key\n",
		"short":     good[:62] + "\n",
		"long":      good + "00\n",
		"uppercase": strings.ToUpper(good) + "\n",
		"zero":      strings.Repeat("00", 32) + "\n",
		"two lines": good + "\n" + good + "\n",
		"crlf":      good + "\r\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := privateDir(t)
			path := filepath.Join(dir, FileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, load := range []func(string) (Key, error){Load, LoadOrCreate} {
				k, err := load(dir)
				if !errors.Is(err, ErrInvalidKey) || !k.IsZero() {
					t.Fatalf("err = %v, key zero = %v; want ErrInvalidKey", err, k.IsZero())
				}
				if good != "" && strings.Contains(err.Error(), good) {
					t.Fatal("error leaks file contents")
				}
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != content {
				t.Fatalf("invalid file was modified: %q, %v", got, err)
			}
			assertNoTemp(t, dir)
		})
	}
}

func TestConcurrentLoadOrCreateConverges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	const n = 32
	keys := make([]Key, n)
	errs := make([]error, n)
	var ready, wg sync.WaitGroup
	ready.Add(1)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Wait()
			keys[i], errs[i] = LoadOrCreate(dir)
		}()
	}
	ready.Done()
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if keys[i] != keys[0] || keys[i].IsZero() {
			t.Fatalf("goroutine %d diverged or got zero key", i)
		}
	}
	if disk, err := Load(dir); err != nil || disk != keys[0] {
		t.Fatalf("on-disk key differs: %v", err)
	}
	assertNoTemp(t, dir)
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

// privateDir returns a fresh 0700 subdirectory. t.TempDir itself may be
// group/world-writable on some harnesses, which production code rightly rejects.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Package localauth manages the per-user secret that authenticates the local
// TokenMunchers proxy to its own clients and control tooling.
//
// A single random 256-bit Key is persisted in the config directory. Three
// independent credentials are derived from it with HMAC-SHA256 under distinct
// domain labels, so disclosure of one derived value never reveals the key or
// either of the other values:
//
//   - RoutePrefix: an unguessable URL path prefix ("/_tm/<capability>") that
//     SDK traffic must use. It is a bearer secret and must be treated like a
//     password (never logged, never sent to untrusted hosts).
//   - ControlToken: the credential for control requests (sent only to a
//     server that has first proven it holds the key).
//   - Proof: a server-to-client HMAC over a client-chosen nonce, which lets a
//     client verify the health endpoint is the genuine local proxy before it
//     ever sends the control token.
//
// # Persistence and platform assumptions
//
// The key lives in <dir>/local-auth.key as 64 lowercase hex characters plus a
// newline. New keys are published atomically: the complete file is written and
// fsynced under a private temporary name and then hard-linked into place, which
// fails if the name already exists. Concurrent processes therefore either see
// no key or a complete one, exactly one creator wins, and every loser adopts
// the winner's key. An existing file that is malformed, all-zero, a symlink,
// not a regular file, or (on Unix) not owner-only and owned by the current
// user is an error and is never overwritten. Filesystems without hard-link
// support are not supported; creation then fails closed.
//
// On Unix the directory must be owned by the current user and not be group- or
// other-writable (it is created 0700 when missing), and the key file must be
// mode 0600-equivalent (no group/other bits). The file name is opened with
// O_NOFOLLOW and every check runs on the opened descriptor. Symlinks in the
// directory path itself and permissions of ancestor directories are NOT
// verified: the config directory is assumed to live under a location the
// current user controls (e.g. the user's home). On other platforms (Windows)
// POSIX modes and ownership are unavailable; the file inherits the ACL of its
// directory, which is assumed to be the user's private profile, and only the
// non-symlink/regular-file checks apply (without TOCTOU protection).
package localauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// KeySize is the size of a Key in bytes.
	KeySize = 32
	// FileName is the key file name inside the config directory.
	FileName = "local-auth.key"

	// NonceLen is the required length of a challenge nonce (lowercase hex of 32 bytes).
	NonceLen = 2 * KeySize

	routePathPrefix = "/_tm/"

	domainRoute   = "tokenmunchers/localauth/v1/route-capability"
	domainControl = "tokenmunchers/localauth/v1/control-token"
	domainProof   = "tokenmunchers/localauth/v1/server-proof"

	// fileLen is the canonical on-disk length: hex key plus newline.
	fileLen = 2*KeySize + 1
)

// ErrInvalidKey reports a key file whose contents or attributes are not
// acceptable. The file is left untouched. Errors never contain key material.
var ErrInvalidKey = errors.New("invalid local auth key")

// Key is the local authentication secret. The zero value is invalid.
type Key [KeySize]byte

// IsZero reports whether k is the (invalid) all-zero key.
func (k Key) IsZero() bool {
	var z Key
	return subtle.ConstantTimeCompare(k[:], z[:]) == 1
}

// Format redacts the key for every fmt verb so it cannot leak via logging.
func (Key) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "localauth.Key(redacted)") }

// MarshalText refuses encoding so the key cannot leak through JSON or similar.
func (Key) MarshalText() ([]byte, error) { return nil, errors.New("localauth: key must not be serialized") }

// RoutePrefix returns "/_tm/<capability>" (no trailing slash), or "" for the
// zero key. The result is a secret.
func (k Key) RoutePrefix() string {
	if k.IsZero() {
		return ""
	}
	return routePathPrefix + hex.EncodeToString(k.mac(domainRoute, nil))
}

// ControlToken returns the hex control credential, or "" for the zero key.
func (k Key) ControlToken() string {
	if k.IsZero() {
		return ""
	}
	return hex.EncodeToString(k.mac(domainControl, nil))
}

// Proof returns the hex server proof for nonce, or "" if the key is zero or
// nonce is not exactly 64 lowercase hex characters.
func (k Key) Proof(nonce string) string {
	if k.IsZero() || !ValidNonce(nonce) {
		return ""
	}
	return hex.EncodeToString(k.mac(domainProof, []byte(nonce)))
}

// VerifyControlToken reports, in constant time, whether got is the control token.
func (k Key) VerifyControlToken(got string) bool {
	want := k.ControlToken()
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// VerifyProof reports, in constant time, whether got is the proof for nonce.
func (k Key) VerifyProof(nonce, got string) bool {
	want := k.Proof(nonce)
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// ValidNonce reports whether nonce is exactly 64 lowercase hex characters.
func ValidNonce(nonce string) bool {
	return len(nonce) == NonceLen && isLowerHex(nonce)
}

// NewNonce returns a fresh random challenge nonce.
func NewNonce() (string, error) {
	var b [KeySize]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (k Key) mac(domain string, data []byte) []byte {
	h := hmac.New(sha256.New, k[:])
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write(data)
	return h.Sum(nil)
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Load reads the key from dir. A missing file yields an error satisfying
// errors.Is(err, fs.ErrNotExist); any other problem wraps ErrInvalidKey or an
// OS error. It never creates or modifies anything.
func Load(dir string) (Key, error) {
	if err := checkDirectory(dir); err != nil {
		return Key{}, err
	}
	return readKey(filepath.Join(dir, FileName))
}

// LoadOrCreate returns the key in dir, atomically creating the directory
// (0700) and a new random key if none exists. Concurrent callers, in this or
// other processes, all return the same key. An existing invalid key file is an
// error and is never replaced.
func LoadOrCreate(dir string) (Key, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Key{}, fmt.Errorf("create local auth dir: %w", err)
	}
	if err := checkDirectory(dir); err != nil {
		return Key{}, err
	}
	path := filepath.Join(dir, FileName)

	key, err := readKey(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}

	if err := publishNew(dir, path); err != nil {
		return Key{}, err
	}
	// Re-read what is actually on disk so all processes converge on it.
	return readKey(path)
}

// publishNew writes a fresh key to a private temp file and links it into
// place. If the final name already exists (another process won), that file is
// left alone and nil is returned; the caller validates it.
func publishNew(dir, path string) (err error) {
	var key Key
	if _, err := rand.Read(key[:]); err != nil {
		return fmt.Errorf("generate local auth key: %w", err)
	}
	if key.IsZero() {
		return fmt.Errorf("generate local auth key: %w", ErrInvalidKey)
	}
	content := []byte(hex.EncodeToString(key[:]) + "\n")

	f, tmp, err := createTemp(dir)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && err == nil {
			err = fmt.Errorf("remove temp local auth key: %w", rerr)
		}
	}()

	_, werr := f.Write(content)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("write local auth key: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("write local auth key: %w", cerr)
	}

	if lerr := os.Link(tmp, path); lerr != nil {
		if errors.Is(lerr, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("publish local auth key: %w", lerr)
	}
	// Best effort durability of the directory entry.
	_ = syncDir(dir)
	return nil
}

func createTemp(dir string) (*os.File, string, error) {
	for range 8 {
		var r [8]byte
		if _, err := rand.Read(r[:]); err != nil {
			return nil, "", fmt.Errorf("create temp local auth key: %w", err)
		}
		tmp := filepath.Join(dir, "."+FileName+"."+hex.EncodeToString(r[:])+".tmp")
		f, err := createExclusive(tmp)
		if err == nil {
			return f, tmp, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", fmt.Errorf("create temp local auth key: %w", err)
		}
	}
	return nil, "", errors.New("create temp local auth key: exhausted attempts")
}

func readKey(path string) (Key, error) {
	var key Key
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return key, fmt.Errorf("local auth key: %w", err)
		}
		return key, fmt.Errorf("open local auth key (symlinks and special files are refused): %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return key, fmt.Errorf("stat local auth key: %w", err)
	}
	if err := checkFile(fi); err != nil {
		return key, fmt.Errorf("%s: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, fileLen+1))
	if err != nil {
		return key, fmt.Errorf("read local auth key: %w", err)
	}
	if err := parseKey(data, &key); err != nil {
		return Key{}, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}

func parseKey(data []byte, key *Key) error {
	if n := len(data); n > 0 && data[n-1] == '\n' {
		data = data[:n-1]
	}
	if len(data) != 2*KeySize || !isLowerHex(string(data)) {
		return fmt.Errorf("%w: malformed key file", ErrInvalidKey)
	}
	if _, err := hex.Decode(key[:], data); err != nil {
		return fmt.Errorf("%w: malformed key file", ErrInvalidKey)
	}
	if key.IsZero() {
		return fmt.Errorf("%w: zero key", ErrInvalidKey)
	}
	return nil
}

func checkDirectory(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("local auth dir: %w", err)
	}
	if err := checkDir(fi); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	return nil
}

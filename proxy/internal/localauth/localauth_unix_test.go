//go:build unix

package localauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCreatedPermissionsAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "cfg")
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm()&0o077 != 0 {
		t.Fatalf("dir mode %v not owner-only", di.Mode().Perm())
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestCreateWorksWithRestrictiveUmask(t *testing.T) {
	dir := privateDir(t) // created before the umask changes
	old := syscall.Umask(0o277)
	defer syscall.Umask(old)
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode = %v, err = %v", fi.Mode().Perm(), err)
	}
}

func TestPermissiveKeyFileRejectedNotRepaired(t *testing.T) {
	dir := privateDir(t)
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	before, _ := os.ReadFile(path)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, load := range []func(string) (Key, error){Load, LoadOrCreate} {
		if _, err := load(dir); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("err = %v, want ErrInvalidKey", err)
		}
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("file modified")
	}
}

func TestWritableDirectoryRejected(t *testing.T) {
	dir := privateDir(t)
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	for _, load := range []func(string) (Key, error){Load, LoadOrCreate} {
		if _, err := load(dir); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("err = %v, want ErrInvalidKey", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("key must not be created in an unsafe directory")
	}
}

func TestExistingPrivateDirectoryWorks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkedKeyFileRefused(t *testing.T) {
	dir := privateDir(t)
	target := filepath.Join(t.TempDir(), "target")
	content := strings.Repeat("ab", 32) + "\n"
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, FileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, load := range []func(string) (Key, error){Load, LoadOrCreate} {
		if k, err := load(dir); err == nil || !k.IsZero() {
			t.Fatalf("symlinked key accepted: %v", err)
		}
	}
	// Dangling symlink must not be written through either.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(t.TempDir(), "missing")
	if err := os.Symlink(dangling, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("dangling symlink accepted")
	}
	if _, err := os.Lstat(dangling); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created file through symlink")
	}
	got, _ := os.ReadFile(target)
	if string(got) != content {
		t.Fatal("symlink target modified")
	}
}

func TestSpecialFileRefusedWithoutBlocking(t *testing.T) {
	dir := privateDir(t)
	if err := syscall.Mkfifo(filepath.Join(dir, FileName), 0o600); err != nil {
		t.Skip("mkfifo unavailable")
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("FIFO accepted")
	}
}

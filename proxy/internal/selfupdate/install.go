package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// digestFor returns the SHA-256 digest SHA256SUMS lists for name. It requires
// exactly one entry for name, carrying 64 lowercase hex digits, as
// install-munch.sh does.
func digestFor(sums []byte, name string) (string, error) {
	var digest string
	entries := 0
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != name {
			continue
		}
		entries++
		digest = fields[0]
	}
	if entries != 1 {
		return "", fmt.Errorf("%s has %d entries for %s, want exactly 1", sumsName, entries, name)
	}
	if !isSHA256Hex(digest) {
		return "", fmt.Errorf("%s has an invalid digest for %s", sumsName, name)
	}
	return digest, nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// extractBinary copies the munch entry of the gzipped tar archive to dst. The
// entry must be unique and a non-empty regular file whose header says it is a
// native executable for goos/goarch. Nothing is run, and the decompressed
// stream may not exceed maxExtracted bytes.
func extractBinary(dst io.Writer, archive io.Reader, goos, goarch string) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(&capReader{r: gz, left: maxExtracted})
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Name != binaryName {
			continue
		}
		if found {
			return fmt.Errorf("archive has more than one %s entry", binaryName)
		}
		found = true
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %s is not a regular file", binaryName)
		}
		if hdr.Size <= 0 {
			return fmt.Errorf("archive entry %s is empty", binaryName)
		}
		if err := copyBinary(dst, tr, goos, goarch); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("archive has no %s entry", binaryName)
	}
	return nil
}

// copyBinary checks the executable header at the start of src and copies all
// of src to dst.
func copyBinary(dst io.Writer, src io.Reader, goos, goarch string) error {
	head := make([]byte, 64) // the ELF64 header, the longer of the two formats
	n, err := io.ReadFull(src, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read %s: %w", binaryName, err)
	}
	head = head[:n]
	if err := checkExecutable(head, goos, goarch); err != nil {
		return fmt.Errorf("%s binary: %w", binaryName, err)
	}
	if _, err := dst.Write(head); err != nil {
		return fmt.Errorf("write %s: %w", binaryName, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy %s: %w", binaryName, err)
	}
	return nil
}

// checkExecutable rejects a file whose header is not that of a 64-bit
// executable for goos/goarch. A wrong-platform binary would pass the checksum
// if it were published that way, and installing it would leave a munch that
// cannot start, and so cannot update itself again.
func checkExecutable(head []byte, goos, goarch string) error {
	switch goos {
	case "linux":
		return checkELF(head, goarch)
	case "darwin":
		return checkMachO(head, goarch)
	}
	return fmt.Errorf("%w (%s)", ErrUnsupportedPlatform, goos)
}

func checkELF(h []byte, goarch string) error {
	if len(h) < 64 || string(h[:len(elf.ELFMAG)]) != elf.ELFMAG {
		return errors.New("not an ELF executable")
	}
	if elf.Class(h[elf.EI_CLASS]) != elf.ELFCLASS64 || elf.Data(h[elf.EI_DATA]) != elf.ELFDATA2LSB {
		return errors.New("not a 64-bit little-endian ELF executable")
	}
	if typ := elf.Type(binary.LittleEndian.Uint16(h[16:18])); typ != elf.ET_EXEC && typ != elf.ET_DYN {
		return fmt.Errorf("ELF file is not an executable (type %d)", uint16(typ))
	}
	want := elf.EM_X86_64
	if goarch == "arm64" {
		want = elf.EM_AARCH64
	}
	if got := elf.Machine(binary.LittleEndian.Uint16(h[18:20])); got != want {
		return fmt.Errorf("built for ELF machine %d, not the %d this platform needs", uint16(got), uint16(want))
	}
	return nil
}

func checkMachO(h []byte, goarch string) error {
	if len(h) < 32 || binary.LittleEndian.Uint32(h[0:4]) != macho.Magic64 {
		return errors.New("not a 64-bit little-endian Mach-O executable")
	}
	if typ := macho.Type(binary.LittleEndian.Uint32(h[12:16])); typ != macho.TypeExec {
		return fmt.Errorf("Mach-O file is not an executable (type %d)", uint32(typ))
	}
	want := macho.CpuAmd64
	if goarch == "arm64" {
		want = macho.CpuArm64
	}
	if got := macho.Cpu(binary.LittleEndian.Uint32(h[4:8])); got != want {
		return fmt.Errorf("built for Mach-O CPU %#x, not the %#x this platform needs", uint32(got), uint32(want))
	}
	return nil
}

// capReader fails once more than left bytes have been read from r, so a small
// archive cannot inflate without bound. Reading exactly left bytes is fine.
type capReader struct {
	r    io.Reader
	left int64
}

var errExtractedTooLarge = fmt.Errorf("archive expands to more than %d bytes", int64(maxExtracted))

func (c *capReader) Read(p []byte) (int, error) {
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, errExtractedTooLarge
	}
	return n, err
}

// target is the regular file an update replaces.
type target struct {
	path string      // absolute, with no symbolic links
	info os.FileInfo // path as the update found it, to notice if it is replaced meanwhile
}

// resolveTarget finds the regular file behind the running executable's path.
func resolveTarget(exe string) (target, error) {
	if exe == "" {
		return target{}, errors.New("cannot locate the running executable")
	}
	path, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return target{}, fmt.Errorf("resolve the running executable: %w", err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return target{}, fmt.Errorf("resolve the running executable: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return target{}, fmt.Errorf("inspect the running executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return target{}, fmt.Errorf("the running executable %s is not a regular file", path)
	}
	return target{path: path, info: info}, nil
}

// staged is the replacement binary under construction, in the directory of
// the file it will replace so that moving it into place is a same-filesystem
// rename.
type staged struct {
	*os.File
	installed bool
}

func newStaged(t target) (*staged, error) {
	dir := filepath.Dir(t.path)
	f, err := os.CreateTemp(dir, ".munch-update-*")
	if err != nil {
		return nil, fmt.Errorf("cannot stage the update (munch needs write access to %s): %w", dir, err)
	}
	return &staged{File: f}, nil
}

// discard removes the staging file unless install moved it into place.
func (s *staged) discard() {
	s.File.Close()
	if !s.installed {
		os.Remove(s.File.Name())
	}
}

// install makes the complete staged binary the file at t.path, keeping that
// file's permission bits. The rename is the only step that changes t.path, and
// it is atomic: the old binary, which a running process may still be executing,
// stays whole until the new one replaces it in a single step.
//
// The download took a while. If t.path is no longer the file the update started
// from, another update or an installer got there first, and install leaves
// that binary alone rather than overwrite it with this release.
func (s *staged) install(t target) error {
	if err := s.Chmod(t.info.Mode().Perm()); err != nil {
		return fmt.Errorf("set permissions on the new binary: %w", err)
	}
	if err := s.Sync(); err != nil {
		return fmt.Errorf("flush the new binary: %w", err)
	}
	if err := s.File.Close(); err != nil {
		return fmt.Errorf("close the new binary: %w", err)
	}
	now, err := os.Lstat(t.path)
	if err != nil {
		return fmt.Errorf("inspect %s before replacing it: %w", t.path, err)
	}
	if !os.SameFile(t.info, now) {
		return fmt.Errorf("%s was replaced while the update was running; nothing was changed, run munch update again", t.path)
	}
	if err := os.Rename(s.Name(), t.path); err != nil {
		return fmt.Errorf("replace %s: %w", t.path, err)
	}
	s.installed = true

	// Make the rename durable. Best effort: not every filesystem can sync a
	// directory, and the replacement is already in place.
	if d, err := os.Open(filepath.Dir(t.path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// newScratch returns a private temporary file to download the archive into. It
// is unlinked the moment it exists, so no way out of the update (a failure, a
// signal, a crash) can leave it behind; the handle works until it is closed.
func newScratch() (*os.File, error) {
	f, err := os.CreateTemp("", "munch-update-*")
	if err != nil {
		return nil, fmt.Errorf("cannot create a temporary file for the download: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot create a temporary file for the download: %w", err)
	}
	return f, nil
}

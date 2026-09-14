package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const copyBufferSize = 1 << 20

// testHooks let package tests inject failures and crashes at exact points.
// Production callers leave the options' hooks nil; every method is nil-safe.
type testHooks struct {
	rename            func(oldpath, newpath string) error
	freeSpace         func(path string) (uint64, error)
	afterSnapshot     func(staging string) error
	atPhase           func(Phase) error
	beforeStagingCopy func() error
	createRollback    func(context.Context, CreateOptions) (*CreateResult, error)
	finalVerify       func() error
	failJournal       func(Phase) error
	failJournalRemove func() error
	openHolders       func(paths []string) ([]int, error)
	binaryHead        int64
}

func (h *testHooks) holders(paths []string) ([]int, error) {
	if h != nil && h.openHolders != nil {
		return h.openHolders(paths)
	}
	return openHolders(paths)
}

// move renames within one filesystem. A cross-device rename is never retried
// as a copy: that would silently give up the atomicity the caller relies on.
func (h *testHooks) move(oldpath, newpath string) error {
	rename := os.Rename
	if h != nil && h.rename != nil {
		rename = h.rename
	}
	if err := rename(oldpath, newpath); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return &Error{Code: CodeCrossDevice, Class: ClassFailed,
				Msg: fmt.Sprintf("%s and %s are on different filesystems; refusing a move that would not be atomic", oldpath, newpath), Err: err}
		}
		return err
	}
	return nil
}

// available reports free bytes for path, and false when that cannot be known.
func (h *testHooks) available(path string) (uint64, bool) {
	fn := freeSpace
	if h != nil && h.freeSpace != nil {
		fn = h.freeSpace
	}
	n, err := fn(path)
	return n, err == nil
}

func (h *testHooks) phase(p Phase) error {
	if h != nil && h.atPhase != nil {
		return h.atPhase(p)
	}
	return nil
}

func (h *testHooks) head() (int64, error) {
	if h != nil && h.binaryHead > 0 {
		return h.binaryHead, nil
	}
	return sqlite.MigrationHead()
}

// lstatNoSymlink never follows a symlink; meeting one is unsafe_path.
func lstatNoSymlink(p string) (os.FileInfo, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, &Error{Code: CodeUnsafePath, Class: ClassRefused, Msg: fmt.Sprintf("%s is a symlink; refusing to follow it", p)}
	}
	return fi, nil
}

// present is lstatNoSymlink with absence as a normal answer.
func present(p string) (os.FileInfo, bool, error) {
	fi, err := lstatNoSymlink(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return fi, true, nil
}

// openRegular opens a regular, non-symlink file and proves the file opened is
// the one inspected, so a swap between the check and the open is detected.
func openRegular(p string) (*os.File, error) {
	li, err := lstatNoSymlink(p)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, &Error{Code: CodeUnsafePath, Class: ClassRefused, Msg: fmt.Sprintf("%s is not a regular file", p)}
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(li, fi) {
		_ = f.Close()
		return nil, &Error{Code: CodeUnsafePath, Class: ClassRefused, Msg: fmt.Sprintf("%s changed while it was being opened", p)}
	}
	return f, nil
}

// ctxReader makes a long copy or hash cancellable between buffers.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// hashFile streams a file through SHA-256; memory use is one buffer.
func hashFile(ctx context.Context, p string) (int64, string, error) {
	f, err := openRegular(p)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.CopyBuffer(h, ctxReader{ctx: ctx, r: f}, make([]byte, copyBufferSize))
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// copyFileHashed copies src to a NEW file dst (never overwriting), hashing
// while it copies, and fsyncs it. A failed copy leaves no dst behind.
func copyFileHashed(ctx context.Context, src, dst string, mode os.FileMode) (n int64, sum string, err error) {
	in, err := openRegular(src)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	h := sha256.New()
	n, err = io.CopyBuffer(io.MultiWriter(out, h), ctxReader{ctx: ctx, r: in}, make([]byte, copyBufferSize))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// The create mode was filtered by the umask; state it exactly.
		err = os.Chmod(dst, mode)
	}
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// writeFileAtomic writes data to a temp file in the same directory, fsyncs it,
// renames it into place and fsyncs the directory: a reader or a crash sees the
// old file or the complete new one, never a partial one.
func writeFileAtomic(p string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return err
	}
	renamed = true
	return syncDir(dir)
}

// syncFile makes a file's contents durable.
func syncFile(p string) error {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	serr := f.Sync()
	return errors.Join(serr, f.Close())
}

// ownerOnly strips group and other bits and guarantees the owner can read and
// write: a backup or restore never relaxes permissions.
func ownerOnly(perm os.FileMode) os.FileMode { return (perm & 0o700) | 0o600 }

//go:build !windows

package upgrade

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func tryLock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
func unlock(f *os.File)        { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func hideWindow(*exec.Cmd) {}

// replace keeps the old binary as PrevPath(dst) and atomically renames src
// over dst. A hard link preserves the old inode while the live path remains
// intact until rename.
func replace(src, dst string) error {
	prev := PrevPath(dst)
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(dst, prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}

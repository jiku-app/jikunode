//go:build windows

package upgrade

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

func tryLock(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

func unlock(f *os.File) {
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

// hideWindow keeps the smoke test from flashing a console when the upgrade
// runs inside the windowless tray app.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// replace swaps src into dst. Windows refuses to delete or overwrite a
// running executable but allows renaming it, so the live binary is renamed to
// PrevPath(dst) first and restored if the final rename fails.
func replace(src, dst string) error {
	prev := PrevPath(dst)
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The previous binary may itself still be running (a tray app that
		// has not restarted yet). Move it out of the way; CleanupStale
		// deletes it once it has exited.
		trash := filepath.Join(filepath.Dir(dst), ".jikunode-old-"+randomSuffix()+".exe")
		if merr := os.Rename(prev, trash); merr != nil {
			return err
		}
	}
	hadOld := true
	if err := os.Rename(dst, prev); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		hadOld = false
	}
	if err := os.Rename(src, dst); err != nil {
		if hadOld {
			_ = os.Rename(prev, dst)
		}
		return err
	}
	return nil
}

//go:build windows

package winapp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// AppDirName is the directory name used under every Windows root.
	AppDirName = "jikunode"
	// NodeExe and TrayExe are the installed program file names.
	NodeExe = "jikunode.exe"
	TrayExe = "jikunode-tray.exe"

	// runKeyPath is the per-user autostart key; RunValueName is our value.
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	RunValueName = "JikuNode"

	// The names are in the session-local namespace, so they only reach the
	// tray app of the same logon session.
	trayMutexName    = `Local\JikuNodeTray`
	quitEventName    = `Local\JikuNodeTrayQuit`
	restartEventName = `Local\JikuNodeTrayRestart`
)

// localAppData returns %LOCALAPPDATA%.
func localAppData() (string, error) {
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		return dir, nil
	}
	return os.UserCacheDir()
}

// InstallDir is where the per-user installer puts both executables:
// %LOCALAPPDATA%\Programs\jikunode (no administrator rights needed).
func InstallDir() (string, error) {
	root, err := localAppData()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "Programs", AppDirName), nil
}

// StateRoot is the root for local, non-roaming state: $XDG_STATE_HOME when
// set (for parity with Unix), otherwise %LOCALAPPDATA%.
func StateRoot() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		return xdg, nil
	}
	return localAppData()
}

// LogDir is where the tray app writes the node's log files.
func LogDir() (string, error) {
	root, err := StateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, AppDirName, "logs"), nil
}

// ---- autostart --------------------------------------------------------------

// SetAutostart makes the tray app at trayPath start, with args, when the user
// logs on. Arguments must not contain quotes or spaces.
func SetAutostart(trayPath string, args ...string) error {
	for _, a := range args {
		if strings.ContainsAny(a, "\" 	") {
			return fmt.Errorf("invalid autostart argument %q", a)
		}
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open autostart key: %w", err)
	}
	defer k.Close()
	return k.SetStringValue(RunValueName, strings.TrimSpace(`"`+trayPath+`" `+strings.Join(args, " ")))
}

// RemoveAutostart deletes the autostart entry and reports whether it existed.
func RemoveAutostart() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open autostart key: %w", err)
	}
	defer k.Close()
	err = k.DeleteValue(RunValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// RemoveFromUserPath drops dir from the per-user PATH the installer extended
// and tells running programs the environment changed.
func RemoveFromUserPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	value, kind, err := k.GetStringValue("Path")
	if err != nil {
		return nil
	}
	var keep []string
	for _, entry := range strings.Split(value, ";") {
		if entry != "" && !strings.EqualFold(filepath.Clean(entry), filepath.Clean(dir)) {
			keep = append(keep, entry)
		}
	}
	joined := strings.Join(keep, ";")
	if joined == value {
		return nil
	}
	if kind == registry.EXPAND_SZ {
		err = k.SetExpandStringValue("Path", joined)
	} else {
		err = k.SetStringValue("Path", joined)
	}
	if err != nil {
		return err
	}
	env := windows.StringToUTF16Ptr("Environment")
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	_, _, _ = procSendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0,
		uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 2000, 0)
	return nil
}

var procSendMessageTimeout = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

// AutostartEnabled reports whether the autostart entry exists.
func AutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(RunValueName)
	return err == nil
}

// ---- single instance and control events -------------------------------------

// Instance is the tray app's single-instance guard and control channel.
type Instance struct {
	mutex   windows.Handle
	Quit    windows.Handle
	Restart windows.Handle
}

// ErrAlreadyRunning is returned by Acquire when another tray app runs.
var ErrAlreadyRunning = errors.New("jikunode-tray is already running")

// Acquire claims the single-instance mutex, retrying for up to wait (a tray
// app that restarts itself releases the mutex just before the new one starts),
// and creates the quit and restart events.
func Acquire(wait time.Duration) (*Instance, error) {
	deadline := time.Now().Add(wait)
	for {
		h, err := windows.CreateMutex(nil, true, windows.StringToUTF16Ptr(trayMutexName))
		if err == nil {
			inst := &Instance{mutex: h}
			if inst.Quit, err = createEvent(quitEventName); err == nil {
				inst.Restart, err = createEvent(restartEventName)
			}
			if err != nil {
				inst.Close()
				return nil, err
			}
			return inst, nil
		}
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return nil, fmt.Errorf("create instance mutex: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, ErrAlreadyRunning
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func createEvent(name string) (windows.Handle, error) {
	// Auto-reset, initially unsignalled.
	h, err := windows.CreateEvent(nil, 0, 0, windows.StringToUTF16Ptr(name))
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 0, fmt.Errorf("create event %s: %w", name, err)
	}
	return h, nil
}

// Close releases the mutex and events so a successor can start.
func (i *Instance) Close() {
	for _, h := range []*windows.Handle{&i.Quit, &i.Restart} {
		if *h != 0 {
			_ = windows.CloseHandle(*h)
			*h = 0
		}
	}
	if i.mutex != 0 {
		_ = windows.ReleaseMutex(i.mutex)
		_ = windows.CloseHandle(i.mutex)
		i.mutex = 0
	}
}

// signal sets a named event and reports whether a tray app was listening.
func signal(name string) bool {
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, windows.StringToUTF16Ptr(name))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h) == nil
}

// TrayRunning reports whether a tray app holds the instance mutex.
func TrayRunning() bool {
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(trayMutexName))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}

// RequestQuit asks a running tray app to stop the node and exit, and waits up
// to wait for it to go. It reports whether a tray app was running.
func RequestQuit(wait time.Duration) bool {
	if !signal(quitEventName) {
		return false
	}
	deadline := time.Now().Add(wait)
	for TrayRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// RequestRestart asks a running tray app to restart itself and the node from
// the binaries now on disk. It reports whether a tray app was running.
func RequestRestart() bool { return signal(restartEventName) }

// ---- processes ----------------------------------------------------------------

// HideConsole makes cmd start without a console window.
func HideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// Detach makes cmd outlive its parent without a console window.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// KillProgramsIn terminates every process (other than this one) whose image
// is one of names inside dir. It returns how many were stopped.
func KillProgramsIn(dir string, names ...string) int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(snap)
	self := uint32(os.Getpid())
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	killed := 0
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ProcessID == self {
			continue
		}
		exe := windows.UTF16ToString(entry.ExeFile[:])
		match := false
		for _, n := range names {
			if strings.EqualFold(exe, n) {
				match = true
			}
		}
		if !match {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, entry.ProcessID)
		if err != nil {
			continue
		}
		path := imagePath(h)
		if path != "" && strings.EqualFold(filepath.Dir(path), filepath.Clean(dir)) {
			if windows.TerminateProcess(h, 1) == nil {
				_, _ = windows.WaitForSingleObject(h, 5000)
				killed++
			}
		}
		_ = windows.CloseHandle(h)
	}
	return killed
}

func imagePath(h windows.Handle) string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

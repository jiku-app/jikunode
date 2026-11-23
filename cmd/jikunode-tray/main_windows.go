//go:build windows

// Command jikunode-tray is the Windows tray app for jikunode. It starts at
// logon (per-user Run key, no administrator rights), runs `jikunode.exe run`
// without a console window, restarts it when it exits, and offers the node's
// state, pause/resume, the status page and logs from the notification area.
//
// Build it with -ldflags -H=windowsgui so no console window opens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"

	"github.com/jiku-app/jikunode/internal/winapp"
	"github.com/jiku-app/jikunode/upgrade"
)

// pollInterval is how often the tray refreshes the node state.
const pollInterval = 3 * time.Second

type app struct {
	inst      *winapp.Instance
	sup       *supervisor
	client    *statusClient
	logger    *log.Logger
	logDir    string
	selfPath  string
	selfStamp fileStamp
	port      int
	// restartSelf makes shutdown start a fresh tray from the binary on disk.
	restartSelf atomic.Bool
	cancel      context.CancelFunc
	done        chan struct{}
}

func main() {
	fs := flag.NewFlagSet("jikunode-tray", flag.ContinueOnError)
	port := fs.Int("port", defaultStatusPort, "loopback status port passed to jikunode run")
	if err := fs.Parse(os.Args[1:]); err != nil || fs.NArg() != 0 || *port <= 0 || *port > 65535 {
		fatal(errors.New("usage: jikunode-tray [--port PORT]"))
	}
	a, err := setup(*port)
	if err != nil {
		if errors.Is(err, winapp.ErrAlreadyRunning) {
			os.Exit(0)
		}
		fatal(err)
	}
	// systray may run onExit on whichever goroutine called Quit while the
	// message loop ends and Run returns, so the real shutdown happens here,
	// on the main goroutine, where the process is sure to wait for it.
	systray.Run(a.onReady, nil)
	a.shutdown()
}

func setup(port int) (*app, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// A tray that restarts itself after an upgrade releases the instance
	// lock just before its successor starts, so wait a little for it.
	inst, err := winapp.Acquire(10 * time.Second)
	if err != nil {
		return nil, err
	}
	logDir, err := winapp.LogDir()
	if err != nil {
		inst.Close()
		return nil, err
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		inst.Close()
		return nil, err
	}
	trayLog, err := openLog(filepath.Join(logDir, "tray.log"))
	if err != nil {
		inst.Close()
		return nil, err
	}
	logger := log.New(io.Writer(trayLog), "", log.LstdFlags)
	// The tray has no console: send any crash report to the log as well.
	_ = debug.SetCrashOutput(trayLog, debug.CrashOptions{})
	upgrade.CleanupStale(self)

	nodePath := filepath.Join(filepath.Dir(self), winapp.NodeExe)
	if _, err := os.Stat(nodePath); err != nil {
		inst.Close()
		return nil, fmt.Errorf("%s not found next to the tray app", winapp.NodeExe)
	}
	a := &app{
		inst:      inst,
		client:    newStatusClient(port),
		port:      port,
		logger:    logger,
		logDir:    logDir,
		selfPath:  self,
		selfStamp: stampOf(self),
		done:      make(chan struct{}),
	}
	a.sup = newSupervisor(nodePath, logDir, logger)
	if port != defaultStatusPort {
		a.sup.args = []string{"--port", strconv.Itoa(port)}
	}
	a.sup.changed = func() bool { return stampOf(a.selfPath) != a.selfStamp }
	a.sup.onSelfUpdate = func() {
		a.restartSelf.Store(true)
		systray.Quit()
	}
	logger.Printf("tray started (pid %d)", os.Getpid())
	return a, nil
}

func (a *app) onReady() {
	icons := map[iconState][]byte{}
	for _, s := range []iconState{iconOff, iconConnecting, iconConnected, iconError} {
		icons[s] = renderIcon(s)
	}
	systray.SetIcon(icons[iconConnecting])
	systray.SetTooltip("JikuNode")

	mTitle := systray.AddMenuItem("JikuNode: starting…", "")
	mTitle.Disable()
	mDetail := systray.AddMenuItem("", "")
	mDetail.Disable()
	mDetail.Hide()
	systray.AddSeparator()
	mPause := systray.AddMenuItem("Pause", "Ask the hub to stop sending traffic to this node")
	mResume := systray.AddMenuItem("Resume", "Ask the hub to send traffic to this node again")
	mResume.Hide()
	mPause.Disable()
	mOpen := systray.AddMenuItem("Open status page", "Open the local status page in your browser")
	mLogs := systray.AddMenuItem("Open log folder", "")
	mRestart := systray.AddMenuItem("Restart node", "")
	systray.AddSeparator()
	mAuto := systray.AddMenuItemCheckbox("Start at login", "Start JikuNode when you sign in to Windows", winapp.AutostartEnabled())
	mQuit := systray.AddMenuItem("Quit JikuNode", "Stop the node and close the tray app")

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go func() {
		a.sup.run(ctx)
		close(a.done)
	}()
	go a.watchEvents(ctx)

	refresh := func() {
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := a.client.status(pctx)
		pcancel()
		v := describe(st, err, a.sup.running())
		systray.SetIcon(icons[v.icon])
		systray.SetTooltip(v.title)
		mTitle.SetTitle(v.title)
		if v.detail != "" {
			mDetail.SetTitle(v.detail)
			mDetail.Show()
		} else {
			mDetail.Hide()
		}
		if v.canResume {
			mPause.Hide()
			mResume.Show()
			mResume.Enable()
		} else {
			mResume.Hide()
			mPause.Show()
			if v.canPause {
				mPause.Enable()
			} else {
				mPause.Disable()
			}
		}
	}
	control := func(action string) {
		cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
		defer ccancel()
		if err := a.client.control(cctx, action); err != nil {
			a.logger.Printf("%s: %v", action, err)
			mTitle.SetTitle("JikuNode: " + truncate(err.Error(), 50))
			return
		}
		refresh()
	}

	go func() {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		refresh()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			case <-mPause.ClickedCh:
				mPause.Disable()
				go control("pause")
			case <-mResume.ClickedCh:
				mResume.Disable()
				go control("resume")
			case <-mOpen.ClickedCh:
				openShell(a.client.pageURL())
			case <-mLogs.ClickedCh:
				openShell(a.logDir)
			case <-mRestart.ClickedCh:
				a.logger.Printf("restart requested from menu")
				a.sup.restart()
			case <-mAuto.ClickedCh:
				a.toggleAutostart(mAuto)
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func (a *app) toggleAutostart(item *systray.MenuItem) {
	if item.Checked() {
		if _, err := winapp.RemoveAutostart(); err != nil {
			a.logger.Printf("disable autostart: %v", err)
			return
		}
		item.Uncheck()
		return
	}
	if err := winapp.SetAutostart(a.selfPath, a.args()...); err != nil {
		a.logger.Printf("enable autostart: %v", err)
		return
	}
	item.Check()
}

// watchEvents reacts to `jikunode uninstall` (quit) and `jikunode upgrade`
// (restart from the new binaries).
func (a *app) watchEvents(ctx context.Context) {
	handles := []windows.Handle{a.inst.Quit, a.inst.Restart}
	for ctx.Err() == nil {
		ev, err := windows.WaitForMultipleObjects(handles, false, 500)
		if err != nil {
			return
		}
		switch ev {
		case windows.WAIT_OBJECT_0:
			a.logger.Printf("quit requested")
			systray.Quit()
			return
		case windows.WAIT_OBJECT_0 + 1:
			a.logger.Printf("restart requested")
			a.restartSelf.Store(true)
			systray.Quit()
			return
		}
	}
}

// shutdown stops the node, releases the instance lock and, after an upgrade,
// starts the new tray binary.
func (a *app) shutdown() {
	if a.cancel != nil {
		a.cancel()
		select {
		case <-a.done:
		case <-time.After(10 * time.Second):
		}
	}
	a.inst.Close()
	if a.restartSelf.Load() {
		cmd := exec.Command(a.selfPath, a.args()...)
		winapp.Detach(cmd)
		if err := cmd.Start(); err != nil {
			a.logger.Printf("restart tray: %v", err)
		} else {
			a.logger.Printf("tray restarting (new pid %d)", cmd.Process.Pid)
		}
	}
	a.logger.Printf("tray stopped")
}

// args are the tray's own arguments, kept for autostart and self-restart.
func (a *app) args() []string {
	if a.port == defaultStatusPort {
		return nil
	}
	return []string{"--port", strconv.Itoa(a.port)}
}

// openShell opens a URL or folder with its default handler.
func openShell(target string) {
	_ = windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(target), nil, nil, windows.SW_SHOWNORMAL)
}

// fatal reports a startup error in a message box: the tray has no console.
func fatal(err error) {
	_, _ = windows.MessageBox(0, windows.StringToUTF16Ptr("JikuNode could not start:\n\n"+err.Error()),
		windows.StringToUTF16Ptr("JikuNode"), windows.MB_OK|windows.MB_ICONERROR)
	os.Exit(1)
}

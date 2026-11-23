//go:build windows

// Command jikunode-tray is the Windows app for jikunode. It starts at sign-in
// (per-user Run key, no administrator rights), runs `jikunode.exe run`
// without a console window and restarts it when it exits. Its window and
// notification-area menu show the node's state and let the user start and
// stop the node, pause and resume it, and choose whether JikuNode starts at
// sign-in and whether it connects as soon as it starts.
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
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"

	"github.com/jiku-app/jikunode/internal/winapp"
	"github.com/jiku-app/jikunode/upgrade"
)

const (
	// pollInterval is how often the tray refreshes the node state.
	pollInterval = 3 * time.Second
	// noticeFor is how long an action's error stays on screen.
	noticeFor = 15 * time.Second
	// backgroundFlag starts the tray without opening its window; the Run key
	// and self-restarts use it.
	backgroundFlag = "--background"
)

type app struct {
	inst         *winapp.Instance
	sup          *supervisor
	client       *statusClient
	logger       *log.Logger
	logDir       string
	settingsPath string
	selfPath     string
	selfStamp    fileStamp
	port         int
	background   bool
	win          *window

	mu        sync.Mutex
	settings  settings
	notice    string
	noticeAt  time.Time
	busy      atomic.Bool // a pause or resume is in flight
	refreshCh chan struct{}

	// restartSelf makes shutdown start a fresh tray from the binary on disk.
	restartSelf atomic.Bool
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

func main() {
	fs := flag.NewFlagSet("jikunode-tray", flag.ContinueOnError)
	port := fs.Int("port", defaultStatusPort, "loopback status port passed to jikunode run")
	background := fs.Bool("background", false, "start in the notification area without opening the window")
	if err := fs.Parse(os.Args[1:]); err != nil || fs.NArg() != 0 || *port <= 0 || *port > 65535 {
		fatal(errors.New("usage: jikunode-tray [--port PORT] [--background]"))
	}
	enableDPIAwareness()
	a, err := setup(*port, *background)
	if err != nil {
		if errors.Is(err, winapp.ErrAlreadyRunning) {
			// Started again from the Start menu: show the running app.
			if !*background {
				winapp.RequestShow()
			}
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

func setup(port int, background bool) (*app, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// A tray that restarts itself releases the instance lock before its
	// successor starts; the short wait covers a slow exit.
	inst, err := winapp.Acquire(3 * time.Second)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*app, error) {
		inst.Close()
		return nil, err
	}
	logDir, err := winapp.LogDir()
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fail(err)
	}
	trayLog, err := openLog(filepath.Join(logDir, "tray.log"))
	if err != nil {
		return fail(err)
	}
	logger := log.New(io.Writer(trayLog), "", log.LstdFlags)
	// The tray has no console: send any crash report to the log as well.
	_ = debug.SetCrashOutput(trayLog, debug.CrashOptions{})
	upgrade.CleanupStale(self)

	nodePath := filepath.Join(filepath.Dir(self), winapp.NodeExe)
	if _, err := os.Stat(nodePath); err != nil {
		return fail(fmt.Errorf("%s not found next to the tray app", winapp.NodeExe))
	}
	// tray.json lives in the jikunode state folder, next to logs\.
	settingsPath := filepath.Join(filepath.Dir(logDir), "tray.json")
	prefs, err := loadSettings(settingsPath)
	if err != nil {
		logger.Printf("settings: %v (using defaults)", err)
	}
	a := &app{
		inst:         inst,
		client:       newStatusClient(port),
		port:         port,
		background:   background,
		logger:       logger,
		logDir:       logDir,
		settingsPath: settingsPath,
		settings:     prefs,
		selfPath:     self,
		selfStamp:    stampOf(self),
		refreshCh:    make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.sup = newSupervisor(nodePath, logDir, logger, prefs.ConnectOnStart)
	if port != defaultStatusPort {
		a.sup.args = []string{"--port", strconv.Itoa(port)}
	}
	a.sup.changed = func() bool { return stampOf(a.selfPath) != a.selfStamp }
	a.sup.onSelfUpdate = func() {
		a.restartSelf.Store(true)
		systray.Quit()
	}
	logger.Printf("tray started (pid %d, connect on start %v)", os.Getpid(), prefs.ConnectOnStart)
	return a, nil
}

// menu holds the notification-area menu items.
type menu struct {
	title, detail                   *systray.MenuItem
	toggle, pause, resume           *systray.MenuItem
	autostart, connect              *systray.MenuItem
	open, page, logs, restart, quit *systray.MenuItem
}

func (a *app) onReady() {
	icons := map[iconState][]byte{}
	for _, s := range []iconState{iconOff, iconConnecting, iconConnected, iconError} {
		icons[s] = renderIcon(s)
	}
	systray.SetIcon(icons[iconConnecting])
	systray.SetTooltip("JikuNode")

	var m menu
	m.open = systray.AddMenuItem("Open JikuNode", "Show the JikuNode window")
	systray.AddSeparator()
	m.title = systray.AddMenuItem("JikuNode: starting…", "")
	m.title.Disable()
	m.detail = systray.AddMenuItem("", "")
	m.detail.Disable()
	m.detail.Hide()
	systray.AddSeparator()
	m.toggle = systray.AddMenuItem("Stop node", "Disconnect from the hub and stop the node")
	m.pause = systray.AddMenuItem("Pause", "Ask the hub to stop sending traffic to this node")
	m.resume = systray.AddMenuItem("Resume", "Ask the hub to send traffic to this node again")
	m.resume.Hide()
	m.pause.Disable()
	m.restart = systray.AddMenuItem("Restart node", "")
	systray.AddSeparator()
	m.page = systray.AddMenuItem("Open status page", "Open the local status page in your browser")
	m.logs = systray.AddMenuItem("Open log folder", "")
	systray.AddSeparator()
	m.autostart = systray.AddMenuItemCheckbox("Start at sign-in", "Start JikuNode when you sign in to Windows", winapp.AutostartEnabled())
	m.connect = systray.AddMenuItemCheckbox("Connect when JikuNode starts", "Start the node and connect to the hub as soon as JikuNode starts", a.connectOnStart())
	systray.AddSeparator()
	m.quit = systray.AddMenuItem("Quit JikuNode", "Stop the node and close JikuNode")

	win, err := newWindow(windowActions{
		toggleNode:   a.toggleNode,
		pause:        func() { a.control("pause") },
		resume:       func() { a.control("resume") },
		openPage:     func() { openShell(a.client.pageURL()) },
		openLogs:     func() { openShell(a.logDir) },
		setAutostart: a.setAutostart,
		setConnect:   a.setConnect,
	}, icons, a.logger)
	if err != nil {
		a.logger.Printf("window: %v (the menu still works)", err)
	}
	a.win = win
	// Left click opens the window; right click shows the menu.
	if win != nil {
		systray.SetOnTapped(win.show)
	}

	go func() {
		a.sup.run(a.ctx)
		close(a.done)
	}()
	go a.watchEvents()

	go func() {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		a.refresh(&m, icons)
		if !a.background {
			a.win.show()
		}
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-t.C:
			case <-a.refreshCh:
			case <-m.open.ClickedCh:
				a.win.show()
				continue
			case <-m.toggle.ClickedCh:
				a.toggleNode()
			case <-m.pause.ClickedCh:
				m.pause.Disable()
				go a.control("pause")
				continue
			case <-m.resume.ClickedCh:
				m.resume.Disable()
				go a.control("resume")
				continue
			case <-m.restart.ClickedCh:
				a.logger.Printf("restart requested from menu")
				a.sup.restart()
			case <-m.page.ClickedCh:
				openShell(a.client.pageURL())
				continue
			case <-m.logs.ClickedCh:
				openShell(a.logDir)
				continue
			case <-m.autostart.ClickedCh:
				a.setAutostart(!m.autostart.Checked())
			case <-m.connect.ClickedCh:
				a.setConnect(!m.connect.Checked())
			case <-m.quit.ClickedCh:
				systray.Quit()
				return
			}
			a.refresh(&m, icons)
		}
	}()
}

// refresh polls the node and updates the icon, the menu and the window.
func (a *app) refresh(m *menu, icons map[iconState][]byte) {
	wanted, up := a.sup.wantRunning(), a.sup.running()
	var st *nodeStatus
	err := errors.New("node not running")
	if wanted || up {
		pctx, pcancel := context.WithTimeout(a.ctx, 2*time.Second)
		st, err = a.client.status(pctx)
		pcancel()
	}
	v := describe(st, err, up, wanted)
	busy := a.busy.Load()
	autostart := winapp.AutostartEnabled()
	connect := a.connectOnStart()
	notice := a.currentNotice()

	systray.SetIcon(icons[v.icon])
	systray.SetTooltip(v.title)
	if notice != "" {
		m.title.SetTitle("JikuNode: " + truncate(notice, 50))
	} else {
		m.title.SetTitle(v.title)
	}
	if v.detail != "" {
		m.detail.SetTitle(v.detail)
		m.detail.Show()
	} else {
		m.detail.Hide()
	}
	if wanted {
		m.toggle.SetTitle("Stop node")
		m.toggle.SetTooltip("Disconnect from the hub and stop the node")
	} else {
		m.toggle.SetTitle("Start node")
		m.toggle.SetTooltip("Start the node and connect to the hub")
	}
	if v.canResume {
		m.pause.Hide()
		m.resume.Show()
		setEnabled(m.resume, !busy)
	} else {
		m.resume.Hide()
		m.pause.Show()
		setEnabled(m.pause, v.canPause && !busy)
	}
	setChecked(m.autostart, autostart)
	setChecked(m.connect, connect)

	ws := windowState{view: v, wanted: wanted, busy: busy, notice: notice, autostart: autostart, connect: connect}
	if st != nil && err == nil {
		ws.nodeID, ws.version = st.NodeID, st.Version
	}
	a.win.update(ws)
}

func (a *app) refreshSoon() {
	select {
	case a.refreshCh <- struct{}{}:
	default:
	}
}

// toggleNode stops a running node or starts a stopped one.
func (a *app) toggleNode() {
	if a.sup.wantRunning() {
		a.sup.stop()
	} else {
		a.sup.start()
	}
	a.refreshSoon()
}

// control sends pause or resume to the node.
func (a *app) control(action string) {
	a.busy.Store(true)
	a.refreshSoon()
	defer func() {
		a.busy.Store(false)
		a.refreshSoon()
	}()
	cctx, ccancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer ccancel()
	if err := a.client.control(cctx, action); err != nil {
		a.logger.Printf("%s: %v", action, err)
		a.setNotice(err.Error())
		return
	}
	a.setNotice("")
}

func (a *app) setAutostart(on bool) {
	defer a.refreshSoon()
	if !on {
		if _, err := winapp.RemoveAutostart(); err != nil {
			a.logger.Printf("disable autostart: %v", err)
			a.setNotice("Could not turn off start at sign-in: " + err.Error())
		}
		return
	}
	if err := winapp.SetAutostart(a.selfPath, append(a.args(), backgroundFlag)...); err != nil {
		a.logger.Printf("enable autostart: %v", err)
		a.setNotice("Could not turn on start at sign-in: " + err.Error())
	}
}

func (a *app) setConnect(on bool) {
	defer a.refreshSoon()
	a.mu.Lock()
	next := a.settings
	next.ConnectOnStart = on
	a.mu.Unlock()
	if err := saveSettings(a.settingsPath, next); err != nil {
		a.logger.Printf("save settings: %v", err)
		a.setNotice("Could not save the setting: " + err.Error())
		return
	}
	a.mu.Lock()
	a.settings = next
	a.mu.Unlock()
	a.logger.Printf("connect on start set to %v", on)
}

func (a *app) connectOnStart() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings.ConnectOnStart
}

func (a *app) setNotice(s string) {
	a.mu.Lock()
	a.notice, a.noticeAt = truncate(s, 90), time.Now()
	a.mu.Unlock()
}

func (a *app) currentNotice() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.noticeAt) > noticeFor {
		a.notice = ""
	}
	return a.notice
}

// watchEvents reacts to `jikunode uninstall` (quit), `jikunode upgrade`
// (restart from the new binaries) and a second launch (show the window).
func (a *app) watchEvents() {
	handles := []windows.Handle{a.inst.Quit, a.inst.Restart, a.inst.Show}
	for a.ctx.Err() == nil {
		ev, err := windows.WaitForMultipleObjects(handles, false, 500)
		if err != nil {
			a.logger.Printf("control events: %v", err)
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
		case windows.WAIT_OBJECT_0 + 2:
			a.win.show()
		}
	}
}

// shutdown stops the node, releases the instance lock and, after an upgrade,
// starts the new tray binary.
func (a *app) shutdown() {
	a.win.destroy()
	a.cancel()
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
	}
	a.inst.Close()
	if a.restartSelf.Load() {
		cmd := exec.Command(a.selfPath, append(a.args(), backgroundFlag)...)
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

func setEnabled(item *systray.MenuItem, on bool) {
	if on {
		item.Enable()
	} else {
		item.Disable()
	}
}

func setChecked(item *systray.MenuItem, on bool) {
	if on != item.Checked() {
		if on {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
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

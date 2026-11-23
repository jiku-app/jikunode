//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jiku-app/jikunode/internal/winapp"
)

const (
	// restartDelay matches RestartSec=5 in the systemd unit.
	restartDelay = 5 * time.Second
	// maxLogSize rotates node.log to node.log.1 when the node starts.
	maxLogSize = 5 << 20
)

// supervisor keeps `jikunode.exe run` alive without a console window, the
// job systemd's Restart=always does on Linux. A node that exits after a
// hub-triggered upgrade is simply started again from the new binary.
type supervisor struct {
	nodePath string
	args     []string // extra arguments for `jikunode run`
	logPath  string
	logger   *log.Logger
	// changed reports whether the tray's own binary was replaced; the
	// supervisor then stops and asks the tray to restart itself.
	changed      func() bool
	onSelfUpdate func()

	mu     sync.Mutex
	proc   *os.Process
	wanted bool          // the node should run (Start) or not (Stop)
	kick   chan struct{} // re-check now, skipping the delay
}

// newSupervisor returns a supervisor that starts the node at once when start
// is true, and otherwise waits for Start.
func newSupervisor(nodePath, logDir string, logger *log.Logger, start bool) *supervisor {
	return &supervisor{
		nodePath: nodePath,
		logPath:  filepath.Join(logDir, "node.log"),
		logger:   logger,
		wanted:   start,
		kick:     make(chan struct{}, 1),
	}
}

// wantRunning reports whether the node should run (it was not stopped).
func (s *supervisor) wantRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wanted
}

// start starts the node and keeps it running.
func (s *supervisor) start() {
	s.mu.Lock()
	s.wanted = true
	s.mu.Unlock()
	s.logger.Printf("node start requested")
	s.poke()
}

// stop stops the node and keeps it stopped until start.
func (s *supervisor) stop() {
	s.mu.Lock()
	s.wanted = false
	s.mu.Unlock()
	s.logger.Printf("node stop requested")
	s.kill()
}

func (s *supervisor) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// running reports whether the node process is alive.
func (s *supervisor) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

// restart stops the node; the loop starts it again at once (and starts a
// stopped node).
func (s *supervisor) restart() {
	s.mu.Lock()
	s.wanted = true
	s.mu.Unlock()
	s.poke()
	s.kill()
}

func (s *supervisor) kill() {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p != nil {
		_ = p.Kill()
	}
}

// run supervises the node until ctx is cancelled, then stops it.
func (s *supervisor) run(ctx context.Context) {
	for {
		if !s.wantRunning() {
			select {
			case <-ctx.Done():
				return
			case <-s.kick:
				continue
			}
		}
		err := s.once(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.logger.Printf("node: %v", err)
		}
		if s.changed != nil && s.changed() {
			s.logger.Printf("tray binary was upgraded; restarting tray")
			if s.onSelfUpdate != nil {
				s.onSelfUpdate()
			}
			return
		}
		if !s.wantRunning() {
			continue // stopped from the tray: wait for Start
		}
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-time.After(restartDelay):
		}
	}
}

// once starts the node, waits for it to exit (or ctx to end) and returns.
func (s *supervisor) once(ctx context.Context) error {
	logFile, err := openLog(s.logPath)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(s.nodePath, append([]string{"run"}, s.args...)...)
	cmd.Dir = filepath.Dir(s.nodePath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	winapp.HideConsole(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.nodePath, err)
	}
	s.mu.Lock()
	s.proc = cmd.Process
	s.mu.Unlock()
	s.logger.Printf("node started (pid %d)", cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		err = <-done
	}
	s.mu.Lock()
	s.proc = nil
	s.mu.Unlock()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		s.logger.Printf("node exited with code %d", exit.ExitCode())
		return nil
	}
	if err == nil {
		s.logger.Printf("node exited")
	}
	return err
}

// openLog appends to path, rotating it first once it exceeds maxLogSize.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// fileStamp identifies a file version by size and modification time.
type fileStamp struct {
	size int64
	mod  time.Time
}

func stampOf(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{info.Size(), info.ModTime()}
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatLogLine(t *testing.T) {
	ts := time.Date(2026, 11, 23, 7, 19, 33, 0, time.Local)
	in := fmt.Sprintf(`time=%s level=INFO msg="jikunode: starting" event=start bound=true`, ts.Format(time.RFC3339Nano))
	if got, want := formatLogLine(in), "07:19:33  INFO   starting  event=start bound=true"; got != want {
		t.Errorf("formatLogLine = %q, want %q", got, want)
	}
	in = fmt.Sprintf(`time=%s level=WARN msg=connected`, ts.Format(time.RFC3339Nano))
	if got, want := formatLogLine(in), "07:19:33  WARN   connected"; got != want {
		t.Errorf("formatLogLine = %q, want %q", got, want)
	}
	plain := "jikunode: listen tcp 127.0.0.1:16888: bind: in use"
	if got := formatLogLine(plain); got != plain {
		t.Errorf("plain line changed: %q", got)
	}
}

func TestReadLogTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	if got, err := readLogTail(path); err == nil || got != "" {
		t.Fatalf("missing file: %q, %v", got, err)
	}
	var b strings.Builder
	for i := 0; i < logTailLines+50; i++ {
		fmt.Fprintf(&b, "line %d\r\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readLogTail(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\r\n")
	if len(lines) != logTailLines || lines[0] != "line 50" || lines[len(lines)-1] != fmt.Sprintf("line %d", logTailLines+49) {
		t.Errorf("tail: %d lines, first %q, last %q", len(lines), lines[0], lines[len(lines)-1])
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readLogTail(path); err != nil || got != "" {
		t.Errorf("empty file: %q, %v", got, err)
	}
}

func TestTrayLogLineRoundTrip(t *testing.T) {
	line := strings.TrimSpace(trayLogLine("WARN", "node exited", "code", 1))
	got := formatLogLine(line)
	if !strings.HasSuffix(got, "WARN   node exited  code=1") {
		t.Errorf("formatted tray line = %q", got)
	}
}

func TestLastLineWriter(t *testing.T) {
	var w lastLineWriter
	fmt.Fprint(&w, "first\nsec")
	fmt.Fprint(&w, "ond\n\n")
	if got := w.Last(); got != "second" {
		t.Errorf("Last = %q", got)
	}
	fmt.Fprint(&w, "unterminated")
	if got := w.Last(); got != "unterminated" {
		t.Errorf("Last = %q", got)
	}
}

func TestFailureView(t *testing.T) {
	v := failureView("jikunode: listen tcp 127.0.0.1:16888: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.", 16888)
	if v.icon != iconError || v.detail != "Port 16888 is used by another program. Change the port below." {
		t.Errorf("port in use: %+v", v)
	}
	v = failureView(`time=2026-11-23T07:19:33Z level=ERROR msg="jikunode: stopped" event=stopped`, 16888)
	if v.detail != "stopped" {
		t.Errorf("slog line: %+v", v)
	}
	v = failureView("jikunode: config is broken", 16888)
	if v.detail != "config is broken" {
		t.Errorf("plain line: %+v", v)
	}
}

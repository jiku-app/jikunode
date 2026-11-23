package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The window's log box shows the end of node.log, made easier to read:
// slog's "time=… level=… msg=…" prefix becomes "15:04:05  INFO  message".

const (
	// logTailBytes and logTailLines bound how much of node.log the window shows.
	logTailBytes = 48 << 10
	logTailLines = 200
)

var slogLine = regexp.MustCompile(`^time=(\S+) level=(\S+) msg=("(?:[^"\\]|\\.)*"|\S+) ?(.*)$`)

// formatLogLine shortens one slog text line; other lines are kept as they are.
func formatLogLine(line string) string {
	m := slogLine.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	stamp := m[1]
	if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
		stamp = t.Local().Format("15:04:05")
	}
	msg := m[3]
	if s, err := strconv.Unquote(msg); err == nil {
		msg = s
	}
	msg = strings.TrimPrefix(msg, "jikunode: ")
	out := fmt.Sprintf("%s  %-5s  %s", stamp, m[2], msg)
	if m[4] != "" {
		out += "  " + m[4]
	}
	return out
}

// readLogTail returns the last lines of the log at path, formatted for the
// log box and joined with CRLF (what a Windows edit control wants).
func readLogTail(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := info.Size() - logTailBytes
	if start < 0 {
		start = 0
	}
	data, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return "", err
	}
	if start > 0 {
		// Drop the partial first line.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
	}
	for i, l := range lines {
		lines[i] = formatLogLine(strings.TrimRight(l, "\r"))
	}
	if len(lines) == 1 && lines[0] == "" {
		return "", nil
	}
	return strings.Join(lines, "\r\n"), nil
}

// trayLogLine is a line the tray adds to node.log, in the node's own format,
// so the log box shows when the node started and stopped.
func trayLogLine(level, msg string, kv ...any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "time=%s level=%s msg=%s", time.Now().Format(time.RFC3339Nano), level, strconv.Quote(msg))
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %v=%v", kv[i], kv[i+1])
	}
	return b.String() + "\n"
}

// lastLineWriter remembers the last complete, non-empty line written to it.
type lastLineWriter struct {
	mu      sync.Mutex
	partial []byte
	last    string
}

func (w *lastLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		if l := strings.TrimSpace(string(w.partial[:i])); l != "" {
			w.last = l
		}
		w.partial = w.partial[i+1:]
	}
	if len(w.partial) > 4096 {
		w.partial = w.partial[len(w.partial)-4096:]
	}
	return len(p), nil
}

func (w *lastLineWriter) Last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if l := strings.TrimSpace(string(w.partial)); l != "" {
		return l
	}
	return w.last
}

// failureView describes a node that exits as soon as it starts; line is the
// last thing it printed and port the status port it was given.
func failureView(line string, port int) view {
	v := view{icon: iconError, title: "JikuNode: node can't start"}
	if m := slogLine.FindStringSubmatch(line); m != nil {
		if s, err := strconv.Unquote(m[3]); err == nil {
			line = s
		} else {
			line = m[3]
		}
	}
	line = strings.TrimPrefix(line, "jikunode: ")
	switch {
	case strings.Contains(line, "Only one usage of each socket address"),
		strings.Contains(line, "address already in use"):
		v.detail = fmt.Sprintf("Port %d is used by another program. Change the port below.", port)
	default:
		v.detail = truncate(line, 70)
	}
	return v
}

package main

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
)

// The CLI must use the status server's route names (pause/resume), not the
// state names: /control/paused matched only the GET / catch-all and answered
// 405, so v0.5.0 could never pause or resume from the command line.
func TestPauseAndResumeUseControlRoutes(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"pause", "/control/pause"},
		{"resume", "/control/resume"},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			got := make(chan string, 1)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Method + " " + r.URL.Path
				w.WriteHeader(http.StatusOK)
			})}
			go func() { _ = srv.Serve(ln) }()
			defer srv.Close()

			port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
			if code := run([]string{tc.cmd, "--port", port}, io.Discard, io.Discard); code != 0 {
				t.Fatalf("%s exit code = %d", tc.cmd, code)
			}
			if g := <-got; g != "POST "+tc.want {
				t.Fatalf("%s requested %s, want POST %s", tc.cmd, g, tc.want)
			}
		})
	}
}

package main

import (
	"bytes"
	"io"
	"github.com/jiku-app/jikunode/internal/bindcode"
	"github.com/jiku-app/jikunode/internal/nodeagent"
	"net/http"
	"strings"
	"testing"
)

type captureHubTransport func(*http.Request) (*http.Response, error)

func (f captureHubTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBindDefaultHubAndOverride(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	code, _ := bindcode.Generate()
	for _, tc := range []struct {
		name, hub string
		args      []string
	}{
		{"default", "https://discovered.example.test", []string{"bind", code}},
		{"override", "https://hub.example.test", []string{"bind", code, "--hub", "https://hub.example.test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			called := false
			discovered := false
			http.DefaultTransport = captureHubTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == nodeagent.NodeConfigURL {
					discovered = true
					if r.Method != "GET" || r.Body != nil {
						t.Fatal("discovery must not send enrollment data")
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"hub_url":"https://discovered.example.test"}`))}, nil
				}
				called = true
				if r.URL.String() != tc.hub+"/v1/node/bind/challenge" {
					t.Errorf("unexpected endpoint: %s", r.URL)
				}
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"bind_code_invalid"}`))}, nil
			})
			var out, errOut bytes.Buffer
			if run(tc.args, &out, &errOut) != 1 || !called {
				t.Fatal("bind did not use the expected hub")
			}
			if discovered != (tc.name == "default") {
				t.Fatal("override must bypass discovery")
			}
		})
	}
}

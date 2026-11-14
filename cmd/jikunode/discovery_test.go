package main

import (
	"context"
	"io"
	"github.com/jiku-app/jikunode/nodeagent"
	"net/http"
	"strings"
	"testing"
)

func TestDiscoveryRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{`{}`, `not json`, `{"hub_url":"http://hub.example"}`, `{"hub_url":"https://user:pass@hub.example"}`, `{"hub_url":"https://hub.example/path"}`, `{"hub_url":"https://hub.example?key=secret"}`} {
		client := &http.Client{Transport: captureHubTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if _, err := nodeagent.DiscoverHub(context.Background(), nodeagent.NodeConfigURL, client); err == nil {
			t.Fatalf("accepted invalid response %s", body)
		}
	}
	client := &http.Client{Transport: captureHubTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	if _, err := nodeagent.DiscoverHub(context.Background(), nodeagent.NodeConfigURL, client); err == nil {
		t.Fatal("accepted unavailable discovery")
	}
}

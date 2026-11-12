package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const NodeConfigURL = "https://dev.jiku.app/api/v1/public/node-config"

func discoverHub(ctx context.Context, endpoint string, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	// Copy the client so callers retain ownership. No cookies, credentials or
	// binding code are included in this public discovery request.
	c := *client
	c.Jar = nil
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" {
			return fmt.Errorf("invalid discovery redirect")
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("node discovery failed; use --hub to override: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("node discovery returned status %d; use --hub to override", resp.StatusCode)
	}
	var cfg struct {
		HubURL string `json:"hub_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&cfg); err != nil {
		return "", fmt.Errorf("invalid node discovery response; use --hub to override")
	}
	u, err := url.Parse(cfg.HubURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("node discovery returned an invalid HTTPS hub URL; use --hub to override")
	}
	return strings.TrimRight(cfg.HubURL, "/"), nil
}

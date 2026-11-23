package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultStatusPort must match DefaultStatusPort in cmd/jikunode.
const defaultStatusPort = 16888

// nodeStatus is the subset of GET /status the tray displays.
type nodeStatus struct {
	NodeID    string `json:"node_id"`
	Version   string `json:"version"`
	HubURL    string `json:"hub_url"`
	Bound     bool   `json:"bound"`
	Connected bool   `json:"connected"`
	Paused    bool   `json:"paused"`
	LastError string `json:"last_error,omitempty"`
}

// statusClient talks to the node's loopback status server only.
type statusClient struct {
	base string
	http *http.Client
}

func newStatusClient(port int) *statusClient {
	return &statusClient{
		base: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		http: &http.Client{Timeout: 3 * time.Second},
	}
}

func (c *statusClient) pageURL() string { return c.base + "/" }

func (c *statusClient) status(ctx context.Context) (*nodeStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status: %s", resp.Status)
	}
	var st nodeStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// control sends pause or resume with the CSRF header the status server wants.
func (c *statusClient) control(ctx context.Context, action string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/control/"+action, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Jikunode", "1")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil && body.Error != "" {
			return fmt.Errorf("%s failed: %s", action, body.Error)
		}
		return fmt.Errorf("%s failed: %s", action, resp.Status)
	}
	return nil
}

// view is what the tray shows for one status poll.
type view struct {
	icon      iconState
	title     string // first menu line and tooltip
	detail    string // second menu line, may be empty
	canPause  bool
	canResume bool
}

// describe maps a status poll to the tray view. err is the poll error;
// nodeUp says whether the supervised node process is alive.
func describe(st *nodeStatus, err error, nodeUp bool) view {
	switch {
	case err != nil && nodeUp:
		return view{icon: iconConnecting, title: "JikuNode: starting…"}
	case err != nil:
		return view{icon: iconOff, title: "JikuNode: stopped", detail: "Restarting shortly"}
	case !st.Bound:
		return view{icon: iconOff, title: "JikuNode: not bound",
			detail: "Run: jikunode bind <code>"}
	case !st.Connected:
		v := view{icon: iconConnecting, title: "JikuNode: connecting…", detail: shortHub(st.HubURL)}
		if st.LastError != "" {
			v.icon = iconError
			v.title = "JikuNode: cannot reach hub"
			v.detail = truncate(st.LastError, 60)
		}
		return v
	case st.Paused:
		return view{icon: iconConnecting, title: "JikuNode: paused", detail: shortHub(st.HubURL), canResume: true}
	default:
		return view{icon: iconConnected, title: "JikuNode: connected", detail: shortHub(st.HubURL), canPause: true}
	}
}

func shortHub(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return truncate(strings.TrimSuffix(u, "/"), 60)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

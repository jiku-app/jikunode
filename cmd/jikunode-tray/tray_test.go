package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestRenderIconIsValidICO(t *testing.T) {
	for _, s := range []iconState{iconOff, iconConnecting, iconConnected, iconError} {
		ico := renderIcon(s)
		var dir [3]uint16
		if err := binary.Read(bytes.NewReader(ico), binary.LittleEndian, &dir); err != nil {
			t.Fatal(err)
		}
		if dir[0] != 0 || dir[1] != 1 || int(dir[2]) != len(iconSizes) {
			t.Fatalf("bad ICONDIR %v", dir)
		}
		for i, size := range iconSizes {
			entry := ico[6+16*i : 6+16*(i+1)]
			if int(entry[0]) != size || int(entry[1]) != size {
				t.Fatalf("entry %d size %dx%d", i, entry[0], entry[1])
			}
			n := binary.LittleEndian.Uint32(entry[8:12])
			off := binary.LittleEndian.Uint32(entry[12:16])
			img, err := png.Decode(bytes.NewReader(ico[off : off+n]))
			if err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
			if img.Bounds().Dx() != size {
				t.Fatalf("frame %d is %d wide", i, img.Bounds().Dx())
			}
		}
	}
}

func TestDescribe(t *testing.T) {
	down := errors.New("connection refused")
	cases := []struct {
		name      string
		st        *nodeStatus
		err       error
		up        bool
		wanted    bool
		icon      iconState
		pause     bool
		resume    bool
		wantTitle string
	}{
		{"starting", nil, down, true, true, iconConnecting, false, false, "JikuNode: starting…"},
		{"crashed", nil, down, false, true, iconOff, false, false, "JikuNode: not running"},
		{"stopped", nil, down, false, false, iconOff, false, false, "JikuNode: stopped"},
		{"stopping", nil, down, true, false, iconOff, false, false, "JikuNode: stopping…"},
		{"unbound", &nodeStatus{}, nil, true, true, iconOff, false, false, "JikuNode: not bound"},
		{"connecting", &nodeStatus{Bound: true}, nil, true, true, iconConnecting, false, false, "JikuNode: connecting…"},
		{"hub error", &nodeStatus{Bound: true, LastError: "dial failed"}, nil, true, true, iconError, false, false, "JikuNode: cannot reach hub"},
		{"paused", &nodeStatus{Bound: true, Connected: true, Paused: true}, nil, true, true, iconConnecting, false, true, "JikuNode: paused"},
		{"connected", &nodeStatus{Bound: true, Connected: true}, nil, true, true, iconConnected, true, false, "JikuNode: connected"},
	}
	for _, c := range cases {
		v := describe(c.st, c.err, c.up, c.wanted)
		if v.icon != c.icon || v.canPause != c.pause || v.canResume != c.resume || v.title != c.wantTitle {
			t.Errorf("%s: got %+v", c.name, v)
		}
	}
}

func TestStatusClient(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_, _ = w.Write([]byte(`{"node_id":"n","bound":true,"connected":true,"hub_url":"https://hub.example"}`))
		case "/control/pause":
			gotHeader = r.Header.Get("X-Jikunode")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"not_connected"}`))
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	c := newStatusClient(p)
	st, err := c.status(context.Background())
	if err != nil || !st.Connected || st.HubURL != "https://hub.example" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := c.control(context.Background(), "pause"); err != nil || gotHeader != "1" {
		t.Fatalf("pause: %v header %q", err, gotHeader)
	}
	if err := c.control(context.Background(), "resume"); err == nil || err.Error() != "resume failed: not_connected" {
		t.Fatalf("resume: %v", err)
	}
}

package main

import "testing"

func TestStatusWaitsForAuthenticatedHandshake(t *testing.T) {
	s := NewStatusServer(StatusOptions{NodeID: "node", Version: "test", Logs: NewLogRing(8)})
	ready := make(chan struct{})
	s.OnConnect(nil, ready)
	if s.Status().Connected {
		t.Fatal("reported connected before authentication")
	}
	close(ready)
	if !s.Status().Connected {
		t.Fatal("missing connected status after authentication")
	}
	s.OnDisconnect(nil)
	if s.Status().Connected {
		t.Fatal("reported connected after disconnect")
	}
}

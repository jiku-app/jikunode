package main

import (
	"context"
	"testing"
	"time"

	"github.com/jiku-app/jikunode/contract"
)

// The node's pause state comes from the hub only.
func TestHubDesiredStateIsTheOnlyPauseSource(t *testing.T) {
	s := NewStatusServer(StatusOptions{NodeID: "node", Version: "test", Logs: NewLogRing(8)})
	if s.Paused() || s.Status().Paused {
		t.Fatal("node must start running")
	}
	s.OnDesiredState(contract.StatePaused)
	if !s.Paused() || !s.Status().Paused {
		t.Fatal("hub pause not applied")
	}
	s.OnDesiredState(contract.StateRunning)
	if s.Paused() || s.Status().Paused {
		t.Fatal("hub resume not applied")
	}
}

// A control action blocks until the hub pushes the state back; it never
// records the requested state itself.
func TestControlWaitsForHubPush(t *testing.T) {
	s := NewStatusServer(StatusOptions{NodeID: "node", Version: "test", Logs: NewLogRing(8)})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	generation := s.pushes.Load()
	done := make(chan error, 1)
	go func() { done <- s.waitState(ctx, true, generation) }()
	select {
	case err := <-done:
		t.Fatalf("returned before the hub pushed the state: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	s.OnDesiredState(contract.StatePaused)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if err := s.waitState(short, false, s.pushes.Load()); err == nil {
		t.Fatal("waitState succeeded for a state the hub never pushed")
	}
}

// A push that arrived before the request must not satisfy it, even when the
// state already matches: success means the hub answered this request.
func TestControlIgnoresEarlierSameStatePush(t *testing.T) {
	s := NewStatusServer(StatusOptions{NodeID: "node", Version: "test", Logs: NewLogRing(8)})
	s.OnDesiredState(contract.StatePaused)
	generation := s.pushes.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.waitState(ctx, true, generation) }()
	select {
	case err := <-done:
		t.Fatalf("an earlier push satisfied the request: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	s.OnDesiredState(contract.StatePaused)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

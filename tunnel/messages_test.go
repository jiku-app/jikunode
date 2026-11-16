package tunnel

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// oversized header: anything above 64 KiB must be refused before allocating.
func TestControlMessageTooLarge_Rejected(t *testing.T) {
	t.Run("reader rejects oversized length before allocating", func(t *testing.T) {
		var buf bytes.Buffer
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(MaxControlMessageSize+1))
		buf.Write(hdr[:])
		var c Control
		err := readFrame(&buf, &c)
		if !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("expected ErrMessageTooLarge, got %v", err)
		}
		// No body bytes were consumed or required: the rejection happened on
		// the length prefix alone.
		if buf.Len() != 0 {
			t.Fatalf("reader consumed body bytes before rejecting")
		}
	})
	t.Run("reader rejects zero length", func(t *testing.T) {
		var buf bytes.Buffer
		var hdr [4]byte
		buf.Write(hdr[:])
		var c Control
		if err := readFrame(&buf, &c); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("expected empty-message error, got %v", err)
		}
	})
	t.Run("writer refuses oversized message", func(t *testing.T) {
		huge := &Control{Drain: &Drain{Reason: strings.Repeat("x", MaxControlMessageSize)}}
		if err := WriteControl(io.Discard, huge); !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("expected ErrMessageTooLarge from writer, got %v", err)
		}
	})
	t.Run("envelope must carry exactly one payload", func(t *testing.T) {
		dual := &Control{Hello: &Hello{}, Heartbeat: &Heartbeat{}}
		if err := dual.Validate(); err == nil {
			t.Fatal("expected error for two payloads")
		}
		if err := (&Control{}).Validate(); err == nil {
			t.Fatal("expected error for zero payloads")
		}
	})
	t.Run("small message round trips", func(t *testing.T) {
		var buf bytes.Buffer
		in := &Control{SetState: &SetState{State: "paused"}}
		if err := WriteControl(&buf, in); err != nil {
			t.Fatalf("write: %v", err)
		}
		out, err := ReadControl(&buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if out.SetState == nil || out.SetState.State != "paused" {
			t.Fatalf("round trip mismatch: %+v", out)
		}
	})
	t.Run("desired state round trips", func(t *testing.T) {
		var buf bytes.Buffer
		in := &Control{DesiredState: &DesiredState{State: "paused"}}
		if err := WriteControl(&buf, in); err != nil {
			t.Fatalf("write: %v", err)
		}
		out, err := ReadControl(&buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if out.DesiredState == nil || out.DesiredState.State != "paused" || out.SetState != nil {
			t.Fatalf("round trip mismatch: %+v", out)
		}
	})
}

func TestHelloWireShapeCarriesNoIdentityKeyOrName(t *testing.T) {
	b, err := json.Marshal(&Control{Hello: &Hello{NodeID: "n", Version: "v", Nonce: []byte{1}, HeartbeatSeconds: 10}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m["hello"]) != 4 {
		t.Fatalf("hello fields: %v", m["hello"])
	}
	for _, k := range []string{"node_id", "version", "nonce", "heartbeat_seconds"} {
		if _, ok := m["hello"][k]; !ok {
			t.Fatalf("missing %s in %s", k, b)
		}
	}
}

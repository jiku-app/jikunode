package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "tray.json")
	s, err := loadSettings(path)
	if err != nil || !s.ConnectOnStart {
		t.Fatalf("missing file: got %+v, %v; want connect on start by default", s, err)
	}
	if err := saveSettings(path, settings{ConnectOnStart: false}); err != nil {
		t.Fatal(err)
	}
	if s, err = loadSettings(path); err != nil || s.ConnectOnStart {
		t.Fatalf("after save: got %+v, %v", s, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err = loadSettings(path); err == nil || !s.ConnectOnStart {
		t.Fatalf("corrupt file: got %+v, %v; want defaults and an error", s, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

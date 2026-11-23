package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// settings are the tray app's own preferences, kept in tray.json next to
// its logs. Starting at sign-in is not stored here: it is the Run key.
type settings struct {
	// ConnectOnStart starts the node (and so connects to the hub) when the
	// tray app starts. When false the tray starts with the node stopped.
	ConnectOnStart bool `json:"connect_on_start"`
	// Port is the node's loopback status port; 0 means the default.
	Port int `json:"port,omitempty"`
}

// validPort reports whether p can be chosen as the status port.
func validPort(p int) bool { return p >= 1024 && p <= 65535 }

func defaultSettings() settings { return settings{ConnectOnStart: true} }

// loadSettings reads path; a missing or unreadable file gives the defaults.
func loadSettings(path string) (settings, error) {
	s := defaultSettings()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultSettings(), err
	}
	return s, nil
}

// saveSettings writes path atomically.
func saveSettings(path string, s settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tray-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

package nodeagent

import (
	"context"
	"errors"
	"github.com/jiku-app/jikunode/tunnel"
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeHook(t *testing.T) {
	for _, test := range []struct {
		name    string
		auto    bool
		hook    bool
		version string
		fail    bool
		want    string
	}{
		{"disabled", false, true, "v2.0.0", false, "declined"}, {"unsupported", true, false, "v2.0.0", false, "unsupported"}, {"invalid", true, true, "garbage", false, "declined"}, {"equal", true, true, "v1.0.0", false, "declined"}, {"failed", true, true, "v2.0.0", true, "started,failed"}, {"success", true, true, "v2.0.0", false, "started"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Runner{Version: "v1.0.0"}
			calls := 0
			if test.hook {
				r.Upgrade = func(context.Context, string) error {
					calls++
					if test.fail {
						return errors.New("secret must never reach network")
					}
					return nil
				}
			}
			states := ""
			err := r.upgradeHandler(&Config{AutoUpgrade: test.auto})(context.Background(), test.version, func(s string) error {
				if states != "" {
					states += ","
				}
				states += s
				return nil
			})
			if states != test.want {
				t.Fatalf("states %s", states)
			}
			if test.name == "success" {
				if !errors.Is(err, tunnel.ErrUpgraded) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if states == "declined" && calls != 0 {
				t.Fatal("called hook")
			}
		})
	}
}

func TestAutoUpgradeConfigDefault(t *testing.T) {
	dir := t.TempDir()
	for _, setting := range []string{"", `,"auto_upgrade":false`, `,"auto_upgrade":true`} {
		data := []byte(`{"hub_url":"https://hub.example.com","node_id":"id","key_path":"key"` + setting + `}`)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AutoUpgrade != (setting != `,"auto_upgrade":false`) {
			t.Fatalf("setting %s", setting)
		}
	}
}

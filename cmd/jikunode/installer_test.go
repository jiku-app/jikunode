package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUserInstallerSystemdAvailability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the systemd installer is a POSIX shell script")
	}
	for _, available := range []bool{true, false} {
		t.Run(map[bool]string{true: "available", false: "unavailable"}[available], func(t *testing.T) {
			home := t.TempDir()
			tools := filepath.Join(home, "tools")
			os.Mkdir(tools, 0700)
			status := "exit 0"
			if !available {
				status = "exit 1"
			}
			os.WriteFile(filepath.Join(tools, "systemctl"), []byte("#!/bin/sh\n"+status+"\n"), 0700)
			src := filepath.Join(home, "source")
			os.WriteFile(src, []byte("binary"), 0700)
			cmd := exec.Command("sh", "../../deploy/jikunode/install-user.sh", src)
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config % quoted"), "PATH="+tools+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != available {
				t.Fatalf("unexpected status: %v %s", err, out)
			}
			if !available && strings.Contains(string(out), "enabled and started") {
				t.Fatal("claimed service running")
			}
			unit, err := os.ReadFile(filepath.Join(home, "config % quoted/systemd/user/jikunode.service"))
			if err != nil {
				t.Fatal(err)
			}
			want := `Environment="XDG_CONFIG_HOME=` + filepath.Join(home, "config %% quoted") + `"`
			if !strings.Contains(string(unit), want) {
				t.Fatalf("unit missing environment %q: %s", want, unit)
			}
		})
	}
}

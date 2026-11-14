package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jiku-app/jikunode/upgrade"
)

func upgradeStateDir() (string, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(root, "jikunode"), nil
}

var runUpgrade = upgrade.Upgrade
var restartUpgradedService = func(ctx context.Context) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", userServiceName).Run(); err != nil {
		return nil
	}
	return exec.CommandContext(ctx, "systemctl", "--user", "restart", userServiceName).Run()
}

func cmdUpgrade(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jikunode upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "exact release tag")
	check := fs.Bool("check", false, "check availability without changing files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "upgrade takes no positional arguments")
		return 2
	}
	dir, err := upgradeStateDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := runUpgrade(context.Background(), upgrade.Options{Version: *version, CurrentVersion: Version, StateDir: dir, Check: *check})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		fmt.Fprintf(stdout, "jikunode %s available=%t\n", result.Version, result.Available)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := restartUpgradedService(ctx); err != nil {
		fmt.Fprintln(stderr, "jikunode: service restart failed")
		return 1
	}
	fmt.Fprintln(stdout, "jikunode upgraded to "+result.Version)
	return 0
}
func autoUpgradeHook() func(context.Context, string) error {
	if upgrade.Supported("") != nil {
		return nil
	}
	dir, err := upgradeStateDir()
	if err != nil {
		return nil
	}
	return func(ctx context.Context, version string) error {
		_, err := upgrade.Upgrade(ctx, upgrade.Options{Version: version, CurrentVersion: Version, StateDir: dir})
		return err
	}
}

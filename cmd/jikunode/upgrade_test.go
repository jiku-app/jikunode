package main

import (
	"bytes"
	"context"
	"github.com/jiku-app/jikunode/upgrade"
	"testing"
)

func TestUpgradeCheckCommand(t *testing.T) {
	old := runUpgrade
	defer func() { runUpgrade = old }()
	runUpgrade = func(ctx context.Context, opts upgrade.Options) (upgrade.Result, error) {
		if !opts.Check || opts.Version != "v2.0.0" {
			t.Fatal(opts)
		}
		return upgrade.Result{Version: opts.Version, Available: true}, nil
	}
	var out bytes.Buffer
	if code := run([]string{"upgrade", "--check", "--version", "v2.0.0"}, &out, &out); code != 0 {
		t.Fatalf("%d %s", code, out.String())
	}
}

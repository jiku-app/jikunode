package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jiku-app/jikunode/contract"
	"github.com/jiku-app/jikunode/nodeagent"
)

// Version is the agent version reported in the tunnel Hello and on the
// status page. Release builds override it with -ldflags -X main.Version=...
var Version = "0.1.0"

// usageText is printed for a missing or unknown subcommand.
const usageText = `jikunode - Jiku supplier egress node

Usage:
  jikunode bind <code> [--hub URL] [--name NAME]   bind this node with a binding code (XXXXX-XXXXX,
                                                   any case, dash optional)
  jikunode run [--port PORT]                       idle until bound, then connect and serve streams
  jikunode pause [--port PORT]                     ask the hub to pause this node
  jikunode resume [--port PORT]                    ask the hub to run this node
  jikunode status [--port PORT]                    print the loopback status JSON
  jikunode logs [--port PORT] [--n N]              print the last N log lines
  jikunode uninstall [--purge]                    remove the user service and binary
  jikunode upgrade [--version vX.Y.Z] [--check]    install a signed release
  jikunode version                                 print the version

A node that is not bound makes no network connection at all. Bind it as the
same user that runs the service: the code is saved in that user's config
directory and the running service picks it up within a few seconds.

The status server listens on 127.0.0.1 only and never accepts or shows a
credential: the node holds none.`

// geteuid is replaced in tests to exercise the root warning.
var geteuid = os.Geteuid

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run dispatches one subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "bind":
		return cmdBind(args[1:], stdout, stderr)
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "pause":
		return cmdControl(args[1:], "pause", contract.StatePaused, stdout, stderr)
	case "resume":
		return cmdControl(args[1:], "resume", contract.StateRunning, stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "logs":
		return cmdLogs(args[1:], stdout, stderr)
	case "upgrade":
		return cmdUpgrade(args[1:], stdout, stderr)
	case "uninstall":
		return cmdUninstall(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "jikunode "+Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "jikunode: unknown subcommand %q\n\n%s\n", args[0], usageText)
		return 2
	}
}

// cmdBind implements: jikunode bind <code> [--hub URL] [--name NAME].
func cmdBind(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jikunode bind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hub := fs.String("hub", "", "hub base URL (defaults to public node discovery)")
	name := fs.String("name", "", "node name (max 40 characters)")

	// The documented form puts the code first, so take a leading positional
	// before handing the rest to flag.Parse.
	var code string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		code = rest[0]
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	switch {
	case code == "" && fs.NArg() == 1:
		code = fs.Arg(0)
	case code == "" && fs.NArg() > 1:
		fmt.Fprintln(stderr, "bind takes exactly one code")
		return 2
	case code != "" && fs.NArg() > 0:
		fmt.Fprintln(stderr, "bind takes exactly one code")
		return 2
	}
	code = strings.TrimSpace(code)
	if code == "" {
		fmt.Fprintln(stderr, usageText)
		return 2
	}

	dir, err := configDir()
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	if geteuid() == 0 {
		fmt.Fprintf(stderr, "jikunode: warning: running as root; this binds root's config directory %s, "+
			"which the user service never reads. Re-run as the user that runs the service.\n", dir)
	}
	cfg, err := nodeagent.Bind(context.Background(), dir, strings.TrimSpace(*hub), code, *name, Version, nodeagent.Network{})
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	path, _ := configPath()
	fmt.Fprintf(stdout, "bound node %s to %s\nwrote %s\nthe running service connects within a few seconds\n", cfg.NodeID, cfg.HubURL, path)
	return 0
}

// cmdRun implements: jikunode run [--port PORT].
func cmdRun(args []string, stdout, stderr io.Writer) int {
	_ = stdout
	fs := flag.NewFlagSet("jikunode run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", DefaultStatusPort, "loopback status page port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, err := configDir()
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	id, _, err := nodeagent.EnsureIdentity(dir)
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}

	ring := NewLogRing(DefaultLogLines)
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(stderr, ring),
		&slog.HandlerOptions{Level: slog.LevelInfo}))
	status := NewStatusServer(StatusOptions{
		NodeID:  id.NodeID,
		Version: Version,
		Port:    *port,
		Logs:    ring,
	})
	ln, err := status.Listen()
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := status.Serve(ctx, ln); err != nil {
			logger.Error("jikunode: status server failed", "event", "status_failed")
		}
	}()
	logger.Info("jikunode: starting", "event", "start",
		"node_id", id.NodeID, "bound", nodeagent.IsBound(dir), "status_addr", ln.Addr().String())

	runner := &nodeagent.Runner{
		Upgrade: autoUpgradeHook(),
		Dir:     dir,
		Version: Version,
		Logger:  logger,
		Hooks:   status,
	}
	err = runner.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("jikunode: stopped", "event", "stopped")
		return 1
	}
	logger.Info("jikunode: stopped", "event", "stopped")
	return 0
}

// cmdControl implements pause and resume against the loopback status server.
// action names the route (/control/<action>); st is the state the hub is
// asked for and the one printed on success.
func cmdControl(args []string, action string, st contract.RunState, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jikunode "+string(st), flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", DefaultStatusPort, "loopback status page port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := controlNode(ctx, *port, action); err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "node state: %s\n", st)
	return 0
}

// cmdStatus implements: jikunode status [--port PORT].
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jikunode status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", DefaultStatusPort, "loopback status page port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := fetchStatus(ctx, *port)
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: encode status: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(out))
	return 0
}

// cmdLogs implements: jikunode logs [--port PORT] [--n N].
func cmdLogs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jikunode logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", DefaultStatusPort, "loopback status page port")
	n := fs.Int("n", 50, "number of lines to print")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := fetchLogs(ctx, *port, *n)
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

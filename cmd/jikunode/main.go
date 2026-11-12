package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jiku-app/jikunode/internal/contract"
)

// Version is the agent version reported in the tunnel Hello and on the
// status page. Release builds override it with -ldflags -X main.Version=...
var Version = "0.1.0"

// usageText is printed for a missing or unknown subcommand.
const usageText = `jikunode - Jiku supplier egress node

Usage:
  jikunode bind <code> [--hub URL] [--name NAME]   register this node with a binding code
  jikunode run [--port PORT]                       connect to the hub and serve streams
  jikunode pause [--port PORT]                     ask the hub to pause this node
  jikunode resume [--port PORT]                    ask the hub to run this node
  jikunode status [--port PORT]                    print the loopback status JSON
  jikunode logs [--port PORT] [--n N]              print the last N log lines
  jikunode uninstall [--purge]                    remove the user service and binary
  jikunode version                                 print the version

The status server listens on 127.0.0.1 only and never accepts or shows a
credential: the node holds none.`

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
		return cmdControl(args[1:], contract.StatePaused, stdout, stderr)
	case "resume":
		return cmdControl(args[1:], contract.StateRunning, stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "logs":
		return cmdLogs(args[1:], stdout, stderr)
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

	hubURL := strings.TrimSpace(*hub)
	if hubURL == "" {
		var err error
		hubURL, err = discoverHub(context.Background(), NodeConfigURL, http.DefaultClient)
		if err != nil {
			fmt.Fprintf(stderr, "jikunode: %v\n", err)
			return 1
		}
	}

	cfg, err := bindNode(context.Background(), hubURL, code, *name)
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "bound node %s to %s\n", cfg.NodeID, cfg.HubURL)
	if path, err := configPath(); err == nil {
		fmt.Fprintf(stdout, "config: %s\n", path)
	}
	fmt.Fprintln(stdout, "next: jikunode run")
	return 0
}

// bindPayload is the POST /v1/node/bind body.
type bindPayload struct {
	Code      string `json:"code"`
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// bindResult is the success body.
type bindResult struct {
	NodeID string `json:"node_id"`
}

// bindNode generates a keypair locally, registers the public key with the hub
// and only then writes the key and the config. A failed bind writes nothing.
func bindNode(ctx context.Context, hubURL, code, name string) (*Config, error) {
	endpoint, err := httpURL(hubURL, bindPath)
	if err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	payload, err := json.Marshal(bindPayload{
		Code:      strings.TrimSpace(code),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
		Name:      strings.TrimSpace(name),
		Version:   Version,
	})
	if err != nil {
		return nil, fmt.Errorf("encode binding request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build binding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact hub %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read hub response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, bindFailure(resp.StatusCode, body)
	}
	var res bindResult
	if err := json.Unmarshal(body, &res); err != nil || strings.TrimSpace(res.NodeID) == "" {
		return nil, errors.New("hub response has no node_id")
	}

	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	if err := ensureConfigDir(dir); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, keyFileName)
	if err := savePrivateKey(keyPath, priv); err != nil {
		return nil, err
	}
	cfg := &Config{
		HubURL:  strings.TrimRight(strings.TrimSpace(hubURL), "/"),
		NodeID:  res.NodeID,
		KeyPath: keyPath,
	}
	if err := saveConfig(cfg); err != nil {
		// Never leave a key behind without the config that identifies it.
		_ = os.Remove(keyPath)
		return nil, err
	}
	return cfg, nil
}

// bindFailure maps a hub refusal to a user-facing error without echoing the
// hub's body beyond its stable error code.
func bindFailure(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	switch {
	case status == http.StatusBadRequest && e.Error == "bind_code_invalid":
		return errors.New("binding code invalid or expired")
	case status == http.StatusTooManyRequests:
		return errors.New("too many binding attempts; wait a minute and retry")
	case status == http.StatusBadRequest:
		return errors.New("hub rejected the binding request")
	default:
		return fmt.Errorf("hub refused the binding (status %d)", status)
	}
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
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}
	key, err := loadPrivateKey(cfg.KeyPath)
	if err != nil {
		fmt.Fprintf(stderr, "jikunode: %v\n", err)
		return 1
	}

	ring := NewLogRing(DefaultLogLines)
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(stderr, ring),
		&slog.HandlerOptions{Level: slog.LevelInfo}))
	status := NewStatusServer(StatusOptions{
		NodeID:  cfg.NodeID,
		Version: Version,
		HubURL:  cfg.HubURL,
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
		"node_id", cfg.NodeID, "status_addr", ln.Addr().String())

	runner := &Runner{
		HubURL:     cfg.HubURL,
		NodeID:     cfg.NodeID,
		Version:    Version,
		PrivateKey: key,
		Logger:     logger,
		Status:     status,
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
func cmdControl(args []string, st contract.RunState, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jikunode "+string(st), flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", DefaultStatusPort, "loopback status page port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := controlNode(ctx, *port, string(st)); err != nil {
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

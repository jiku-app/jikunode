package nodeagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jiku-app/jikunode/bindcode"
)

const (
	bindTimeout         = 20 * time.Second
	maxName             = 40
	bindProofDomain     = "jiku-bind-proof-v2:"
	bindPoolProofDomain = "jiku-bind-pool-proof-v1:"
	maxPoolSecret       = 256 // bytes, matching the hub's bind limit
)

type bindRequest struct {
	NodeID     string `json:"node_id"`
	PublicKey  []byte `json:"public_key"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Code       string `json:"code"`
	PoolSecret string `json:"pool_secret,omitempty"`
	Nonce      []byte `json:"nonce"`
	Signature  []byte `json:"signature"`
}

// ErrBindCodeFormat is returned before any network call for a code that fails
// local validation (length, alphabet or checksum).
var ErrBindCodeFormat = errors.New("binding code is not valid; expected the form XXXXX-XXXXX")

// Bind registers this node with the hub using a binding code. The code is
// validated locally first (a typo or an old 6-character code fails without a
// network call), the identity is created offline, the hub is discovered only
// when hubURL is empty, and the bound marker is written last. name defaults to
// the host name.
func Bind(ctx context.Context, dir, hubURL, code, name, version string, network Network) (*Config, error) {
	normalized, err := bindcode.Normalize(code)
	if err != nil {
		return nil, ErrBindCodeFormat
	}
	return bind(ctx, dir, hubURL, name, version, bindRequest{Code: normalized}, network)
}

// BindPool registers this node as an operator-managed pool agent. secret must
// be the hub's pool bind secret, without surrounding file whitespace.
func BindPool(ctx context.Context, dir, hubURL, secret, name, version string, network Network) (*Config, error) {
	if secret == "" || len(secret) > maxPoolSecret || !utf8.ValidString(secret) {
		return nil, errors.New("pool bind secret must be valid UTF-8 and 1 to 256 bytes")
	}
	return bind(ctx, dir, hubURL, name, version, bindRequest{PoolSecret: secret}, network)
}

func bind(ctx context.Context, dir, hubURL, name, version string, req bindRequest, network Network) (*Config, error) {
	name, err := nodeName(name)
	if err != nil {
		return nil, err
	}
	id, key, err := EnsureIdentity(dir)
	if err != nil {
		return nil, err
	}
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if hubURL == "" {
		hubURL, err = DiscoverHub(ctx, NodeConfigURL, network.Client(nil))
		if err != nil {
			return nil, err
		}
	}
	if _, err = TunnelURL(hubURL); err != nil {
		return nil, err
	}
	cfg := &Config{AutoUpgrade: true, Name: name, HubURL: hubURL, NodeID: id.NodeID, KeyPath: id.KeyPath}
	if hasBinding(dir) {
		existing, err := LoadConfig(dir)
		if err != nil {
			return nil, err
		}
		cfg.PoolPrefixes = existing.PoolPrefixes
	}
	req.NodeID, req.Name, req.Version = cfg.NodeID, cfg.Name, version
	if err = bindNode(ctx, cfg.HubURL, key, req, network); err != nil {
		return nil, err
	}
	if err = saveConfig(dir, cfg); err != nil {
		return nil, err
	}
	if err = writeFileAtomic(filepath.Join(dir, boundFileName), []byte("bound\n")); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ResetBinding forgets the binding and the node identity (bound marker,
// config, node id and key) after the hub rejected the node as unknown or
// revoked. The marker goes first so the runner idles at once; the next bind
// creates a fresh identity, which a revoked id could never get back.
func ResetBinding(dir string) error {
	var firstErr error
	for _, name := range []string{boundFileName, configFileName, idFileName, keyFileName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func nodeName(requested string) (string, error) {
	name := strings.TrimSpace(requested)
	if name != "" {
		if len(name) > maxName {
			return "", errors.New("node name exceeds 40 bytes")
		}
		return name, nil
	}
	name, _ = os.Hostname()
	name = strings.TrimSpace(name)
	if name == "" {
		return "jikunode", nil
	}
	if len(name) > maxName {
		name = name[:maxName]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	return name, nil
}

func bindNode(ctx context.Context, hubURL string, key ed25519.PrivateKey, bind bindRequest, network Network) error {
	ctx, cancel := context.WithTimeout(ctx, bindTimeout)
	defer cancel()
	client := network.Client(nil)
	pool := bind.PoolSecret != ""
	if pool {
		// A 307 or 308 could otherwise replay the secret to another endpoint.
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	post := func(path string, payload any) ([]byte, error) {
		endpoint, err := HTTPURL(hubURL, path)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if pool {
				// A malformed redirect can put a hub-controlled Location in the error.
				return nil, errors.New("could not contact hub for pool binding")
			}
			return nil, fmt.Errorf("contact hub: %w", err)
		}
		defer resp.Body.Close()
		result, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			if pool {
				return nil, errors.New("could not read hub pool bind response")
			}
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, bindFailure(resp.StatusCode, result)
		}
		return result, nil
	}
	body, err := post(bindPath+"/challenge", struct{}{})
	if err != nil {
		return err
	}
	var challenge struct {
		Nonce []byte `json:"nonce"`
	}
	if json.Unmarshal(body, &challenge) != nil || len(challenge.Nonce) != 32 {
		return errors.New("invalid binding challenge")
	}
	domain, credential := bindProofDomain, bind.Code
	if pool {
		domain, credential = bindPoolProofDomain, bind.PoolSecret
	}
	bind.PublicKey = key.Public().(ed25519.PublicKey)
	bind.Nonce = challenge.Nonce
	msg := append([]byte(domain+bind.NodeID), bind.Nonce...)
	msg = append(msg, bind.PublicKey...)
	msg = append(msg, credential...)
	bind.Signature = ed25519.Sign(key, msg)
	body, err = post(bindPath, bind)
	if err != nil {
		return err
	}
	var result struct {
		NodeID string `json:"node_id"`
	}
	if json.Unmarshal(body, &result) != nil || result.NodeID != bind.NodeID {
		return errors.New("hub returned an invalid node identity")
	}
	return nil
}

// bindFailure maps a hub refusal to a user-facing error without echoing the
// hub's body beyond its stable error code.
func bindFailure(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	switch {
	case e.Error == "bind_code_invalid":
		return errors.New("binding code invalid")
	case e.Error == "pool_bind_secret_invalid":
		return errors.New("hub rejected the pool bind secret")
	case e.Error == "bind_challenge_invalid":
		return errors.New("binding challenge expired or was rejected; retry")
	case e.Error == "node_already_bound":
		return errors.New("node is already bound to another owner")
	case e.Error == "node_key_mismatch":
		return errors.New("hub already holds a different key for this node")
	case e.Error == "node_revoked":
		return errors.New("this node was revoked by its owner")
	case status == http.StatusTooManyRequests:
		return errors.New("too many binding attempts; wait a minute and retry")
	case status == http.StatusBadRequest:
		return errors.New("hub rejected the binding request")
	default:
		return fmt.Errorf("hub refused the binding (status %d)", status)
	}
}

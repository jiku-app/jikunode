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

	"github.com/jiku-app/jikunode/internal/bindcode"
)

const (
	bindTimeout = 20 * time.Second
	maxName     = 40
	// bindProofDomain prefixes the signed bind proof.
	bindProofDomain = "jiku-bind-proof-v2:"
)

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
	name, err = nodeName(name)
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
	cfg := &Config{Name: name, HubURL: hubURL, NodeID: id.NodeID, KeyPath: id.KeyPath}
	if err = bindNode(ctx, cfg, key, normalized, version, network); err != nil {
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

func bindNode(ctx context.Context, cfg *Config, key ed25519.PrivateKey, code, version string, network Network) error {
	ctx, cancel := context.WithTimeout(ctx, bindTimeout)
	defer cancel()
	post := func(path string, payload any) ([]byte, error) {
		endpoint, err := HTTPURL(cfg.HubURL, path)
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
		resp, err := network.Client(nil).Do(req)
		if err != nil {
			return nil, fmt.Errorf("contact hub: %w", err)
		}
		defer resp.Body.Close()
		result, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
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
	pub := key.Public().(ed25519.PublicKey)
	msg := append([]byte(bindProofDomain+cfg.NodeID), challenge.Nonce...)
	msg = append(msg, pub...)
	msg = append(msg, code...)
	body, err = post(bindPath, struct {
		NodeID    string `json:"node_id"`
		PublicKey []byte `json:"public_key"`
		Name      string `json:"name"`
		Version   string `json:"version"`
		Code      string `json:"code"`
		Nonce     []byte `json:"nonce"`
		Signature []byte `json:"signature"`
	}{cfg.NodeID, pub, cfg.Name, version, code, challenge.Nonce, ed25519.Sign(key, msg)})
	if err != nil {
		return err
	}
	var result struct {
		NodeID string `json:"node_id"`
	}
	if json.Unmarshal(body, &result) != nil || result.NodeID != cfg.NodeID {
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

# Jiku node protocol

The wire protocol between a supplier's **JikuNode** agent (`cmd/jikunode`)
and the jiku hub (`internal/hub`, running in-process inside `jikud` on
`JIKU_HUB_ADDR`, default `127.0.0.1:8319`). This document describes the
protocol as implemented; the code (`internal/tunnel`, `internal/hub`,
`cmd/jikunode`) is the source of truth.

The node **dials out** to the hub, so it works behind NAT. The node is a pure
byte pipe: TLS to the upstream always starts inside `jikud`, the node never
holds a credential and never sees plaintext. The only node-side secret is its
own ed25519 private key (`$XDG_CONFIG_HOME/jikunode/node.key`, mode `0600`).

## Transport

1. The node opens a WebSocket to `GET /v1/node/tunnel` (binary messages;
   `http://` hub URLs upgrade to `ws://`, `https://` to `wss://`). The
   websocket dial is bounded by 15 s.
2. Both sides run [yamux](https://github.com/hashicorp/yamux) over the
   WebSocket connection (yamux logging is discarded so nothing from the
   multiplexer can reach stdout).
3. The **first yamux stream is the control stream**; the node opens it. Every
   later stream opened by the hub is a data stream.

## Framing

Control messages and data-stream headers are length-prefixed JSON: a 4-byte
big-endian length followed by the UTF-8 JSON body. One framed message larger
than **64 KiB** (`tunnel.MaxControlMessageSize`) is rejected **before any
buffer is allocated**.

## Control stream

The control envelope carries exactly one payload (`tunnel.Control`); any other
shape fails the session:

| Message | Direction | Fields | Notes |
|---|---|---|---|
| `hello` | node → hub | `node_id` (uuid), `version` (≤ 64 chars), `nonce` (32 bytes) | must be the hub's first control read, within 5 s |
| `challenge` | hub → node | `nonce` (32 bytes, crypto/rand) | issued through a per-hub `NonceLog` that refuses duplicate nonces |
| `proof` | node → hub | `signature` (64 bytes) | ed25519 signature, below |
| `heartbeat` | both | — | node sends one every 10 s; the hub's first heartbeat doubles as the handshake ack; the hub fails the session after 30 s without one |
| `set_state` | node → hub | `state`: `running` \| `paused` | node-initiated pause/run; a persistence error fails the session so the node reconnects instead of trusting a lost state change |
| `drain` | hub → node | `reason` | graceful disconnect (e.g. hub shutdown); the node treats it as `tunnel.ErrDrained` |

### Handshake

```
node                          hub
 ├── open control stream ────►│
 ├── hello ──────────────────►│  (node_id, version, nonce_N)
 │                            ├── look up the node's ed25519 public key
 │                            │   (revoked/unknown → close immediately)
 │◄───────────────────────────┤── challenge (nonce_C)
 ├── proof ──────────────────►│  ed25519 over the signature message
 │                            ├── verify, register the session
 │◄───────────────────────────┤── heartbeat (ack)
```

The proof's signed byte string is
`tunnel.SignatureMessage = "jiku-tunnel-proof-v1:" || nonce_N || nonce_C`
(domain separator ‖ hello nonce ‖ challenge nonce, both exactly 32 bytes).
The domain separator keeps tunnel proofs distinct from every other ed25519
use in the system. Replay defence is layered: each session verifies the
signature over its own fresh nonces, and the hub's `NonceLog` refuses to ever
issue the same challenge nonce twice.

## Presence

A registered session upserts a row in `node_sessions`
(`node_id`, `hub_instance_id`, `session_id`, `connected_at`, `lease_until`)
with a **30 s lease**, renewed every 10 s by the session's renew loop. A
renewal that updates zero rows (a newer session displaced this node) or any
renewal error fails the session — presence that cannot be proven does not
count (fail closed).

- The hub sweeps expired presence rows every 10 s (rows left behind by
  crashed hub instances).
- **Newest session wins**: a second successful handshake for the same node
  closes the older session.
- Supply mutations commit `NOTIFY jiku_egress`; the hub listens and closes
  the live session of a revoked or deleted node immediately.

## Data streams

The hub opens one yamux stream per egress connection:

1. Hub writes the framed stream header:
   `{"v":1,"target_host":"api.anthropic.com","target_port":443}`
   (`tunnel.StreamHeader`; `v` must be `1`, the only accepted version).
2. The node reads the header within 10 s, resolves `target_host`, applies the
   destination policy (below) and dials the checked IP.
3. The node answers with a 1-byte dial status within 5 s:
   `0x00` = connected; `0x01` = error, followed by one code byte:
   `1` refused by the destination policy, `2` dial failed, `3` dial timeout.
4. On `0x00`, raw bytes flow in both directions until either side closes;
   half-closes propagate (EOF towards the hub half-closes the target and vice
   versa), and the session's close force-closes the target so a silent peer
   cannot pin a goroutine.

The target host travels the wire (the node must know where to connect) but is
never logged on either side.

## Node-side destination policy

After DNS resolution the node refuses the dial when **any** resolved address
is private, loopback, link-local, CGNAT (100.64.0.0/10), IPv6 ULA,
unspecified, multicast or the IPv4 broadcast address
(`tunnel.DialPolicy`). It then dials the **checked IP directly — never the
hostname** — so the DNS answer cannot change between the check and the dial.
There is deliberately no host or port allowlist (owner decision); this
post-resolution private-address refusal is the only node-side destination
policy. The dial timeout is 10 s.

## Binding (identity onboarding)

A node identity is created out-of-band, before any tunnel:

- The supplier accepts the provider terms once, then retrieves a reusable
  **binding code** from the provider console (`POST /api/v1/supplier/bind-codes`).
  Codes contain five random uppercase alphanumeric characters and one checksum
  character (six total). They are stored uniquely per user, reusable and do not
  expire. Lowercase input is normalized. The checksum detects single-character
  substitutions, not transpositions. These are account-routing identifiers,
  not console credentials; no console approval is required for enrollment.
- `jikunode bind <code> --hub <url> --name <name>` generates the ed25519
  keypair locally and calls:

  ```
  POST /v1/node/bind
  {"code":"...","public_key":"<base64 std of the 32-byte ed25519 public key>",
   "name":"kitchen-pi","version":"0.1.0"}
  → 200 {"node_id":"<uuid>"}
  ```

  On success only, the node writes its config and key locally (atomic,
  mode 0600; a failed bind writes nothing).
- Hub-side limits: JSON body ≤ 4 KiB, 10 s authority timeout, code ≤ 64 and
  name ≤ 40 characters, rate limit 10 attempts/min per remote IP (fail-closed
  limiter memory). Errors are the closed set `invalid_request`,
  `bind_code_invalid`, `rate_limited`, `internal_error` — nothing else is ever
  echoed.

Proxy nodes (`type='proxy'`) do not use this protocol at all: jikud dials the
supplier's HTTP CONNECT / socks5h proxy directly, and reachability comes from
the 30 s health-check loop (`internal/egress/checks`) instead of a tunnel
lease.

## Reconnect behaviour

`jikunode run` reconnects with jittered exponential backoff from 1 s to 60 s.
The backoff resets only after a session has stayed up for 60 s, so a flapping
hub cannot drive a reconnect storm.

## Hub-side observability and logging

Tunnel metrics (session gauge, reconnects, expired leases, heartbeat RTT) are
exported through the OBS registry. Logging discipline (OBS S10): only node
ids and event names are ever logged — payload bytes, dial targets, proxy URLs
and secrets never appear.

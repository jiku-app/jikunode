# Jiku node protocol

The wire protocol between a supplier's **JikuNode** agent (`cmd/jikunode`)
and the jiku hub (`internal/hub`, running in-process inside `jikud` on
`JIKU_HUB_ADDR`, default `127.0.0.1:8319`). This document describes the
protocol as implemented; the code (`internal/tunnel`, `internal/hub`,
`internal/nodeagent`, `cmd/jikunode`, `mobile`) is the source of truth.

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
   multiplexer can reach stdout). yamux keepalive is disabled on both sides;
   the control heartbeat alone detects a dead peer.
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
| `hello` | node → hub | `node_id` (canonical UUID), `version` (≤ 64 bytes), `nonce` (32 bytes), `heartbeat_seconds` (required integer, 5..300) | must be the hub's first control read, within 5 s; invalid heartbeat interval closes the session. There is no `public_key` or `name`: both were registered at bind time |
| `challenge` | hub → node | `nonce` (32 bytes, crypto/rand) | issued through a per-hub `NonceLog` that refuses duplicate nonces |
| `proof` | node → hub | `signature` (64 bytes) | ed25519 signature, below |
| `heartbeat` | hub → node → hub | — | hub sends one each declared interval (`heartbeat_seconds`; CLI 10 s, mobile 60 s); node immediately echoes it; the handshake ack is not echoed. Either side fails after 3 intervals without the peer heartbeat/echo; RTT measures send-to-echo time. The presence lease follows this negotiated window. Dead peers count as present for up to 3 intervals |
| `set_state` | node → hub | `state`: `running` \| `paused` | node-initiated pause/run; a persistence error fails the session so the node reconnects instead of trusting a lost state change |
| `drain` | hub → node | `reason` | graceful disconnect (e.g. hub shutdown); the node treats it as `tunnel.ErrDrained` |

### Handshake

```
node                          hub
 ├── open control stream ────►│
 ├── hello ──────────────────►│  (node_id, version, nonce_N, heartbeat_seconds)
 │                            ├── look the node up: unknown or revoked -> close
 │◄───────────────────────────┤── challenge (nonce_C)
 ├── proof ──────────────────►│  ed25519 over the signature message
 │                            ├── verify key possession (key stored at bind), register session
 │◄───────────────────────────┤── heartbeat (ack)
```

The hub never registers a node on connect: only a node that completed a bind
(below) has a row, so there are no unbound sessions and no unauthenticated
inventory. The proof's signed byte string is
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

A node that has not accepted a binding code is **idle**: `jikunode run` and the
Android core start, report state `unbound`, poll the local `bound` marker file
every 2 s and make **no network connection at all** (no tunnel, no discovery,
no DNS). The identity (UUID and ed25519 key) is created offline on first start.
A node row exists only after a valid code was presented, so the hub has no
unbound sessions or inventory.

The supplier obtains a fixed, reusable, rotatable binding code in the provider
console after accepting provider terms, shown as `XXXXX-XXXXX`: 10 Crockford
base32 symbols (`0-9A-Z` without `I L O U`), 9 random plus 1 Luhn mod 32 check
symbol. Normalization (`internal/bindcode`) strips whitespace and `-`,
upper-cases and maps `O`->`0`, `I`/`L`->`1`; anything else, any other length
and a bad checksum is invalid. Old 6-character codes are invalid. The CLI and
app reject typos locally; the hub rejects a bad checksum as `invalid_request`
before any database lookup.

Binding is plain HTTP, no tunnel:

1. `POST /v1/node/bind/challenge` with `{}` returns `{ "nonce": "<base64 32
   bytes>" }`. The nonce is **stateless**: `expiry_unix (8 bytes, big endian) ||
   HMAC-SHA256(hubKey, "jiku-bind-nonce-v1:" || expiry)[:24]`, valid 60 s,
   `hubKey` random per hub process. The hub keeps no per-challenge state; a
   replay inside 60 s is harmless because an identical bind is idempotent.
2. `POST /v1/node/bind` (JSON, at most 4 KiB, no trailing data) with
   `{node_id, public_key, name, version, code, nonce, signature}`: `public_key`,
   `nonce` and `signature` are base64 (32, 32 and 64 bytes), `name` at most 40
   and `version` at most 64 bytes, `code` as typed. `signature` is ed25519 by
   the node key over `"jiku-bind-proof-v2:" || node_id (36 ASCII) || nonce (32
   raw) || public_key (32 raw) || normalized code (10 ASCII)`, so it proves
   possession of the key being registered.
3. Hub order: parse; normalize and checksum (`invalid_request`, no DB); nonce
   MAC and expiry (`bind_challenge_invalid`); signature against the supplied
   `public_key`; code lookup (`bind_code_invalid`); one transaction. Unknown
   `node_id`: insert the agent bound to the code's supplier. Existing node,
   same key and supplier: idempotent success. Key differs:
   `409 node_key_mismatch`. Other supplier: `409 node_already_bound`. Revoked:
   `403 node_revoked`. A new node beyond 100 agent nodes for the supplier (revoked
   rows included) is `409 node_limit_reached`; the check and the insert run under
   the user's bind-code advisory lock, which also makes a bind lose against a
   concurrent code rotation (`bind_code_invalid`). Success returns `{ "node_id": "<uuid>" }`; there is no
   drain because a bind never has a live session.
4. Throttles are **reserve-then-refund**: after the checksum passes, one token
   is reserved atomically from the per-client-IP bucket (10/min) and the global
   bucket (120/min) before any signature or database work (`429 rate_limited`
   when empty), and refunded unless the request failed (`bind_code_invalid`,
   `bind_challenge_invalid`, bad signature). Successful binds and checksum
   rejections net-consume nothing. A per-code limit of 30 binds/min (in-memory,
   bounded) answers `429 rate_limited`. Challenge requests have their own 60/min
   per-bucket cap. The client IP is resolved through trusted proxies only. The
   global cap can delay onboarding under attack (documented edge case).
5. The tunnel limiter counts only failed or rejected handshakes (bad hello,
   unknown or revoked node, bad proof; 10/min per client-IP bucket); successful
   handshakes and reconnects are refunded and never throttled. The hub allows at
   most 3 outstanding unechoed heartbeats; echoes lagging by more than 3
   intervals fail the session.

Node-side order (`nodeagent.Bind`): validate the code locally (no network on
failure) -> create the identity offline if missing (exclusive atomic create, so
the idle service and the CLI never overwrite each other; the loser reloads the
winner's files) -> discover the hub only when no hub URL was given -> challenge
-> signed bind -> write `config.json` (`name`, `hub_url`, `node_id`, `key_path`)
-> write the `bound` marker **last**. The name defaults to the host name
(Android passes the device model). All traffic uses the injected network
resolver. The idle runner sees the marker, reloads config and key from disk and
connects with the usual backoff, without a restart. The hub URL is therefore
discovered at bind time, never at run time. A failed bind keeps the identity for
reuse and writes no config or marker.

Proxy nodes (`type='proxy'`) do not use this protocol at all: jikud dials the
supplier's HTTP CONNECT / socks5h proxy directly, and reachability comes from
the 30 s health-check loop (`internal/egress/checks`) instead of a tunnel
lease.

## Reconnect behaviour

`jikunode run` (once bound) reconnects with jittered exponential backoff from 1 s to 60 s.
The backoff resets only after a session has stayed up for 60 s, so a flapping
hub cannot drive a reconnect storm. A node deleted on the hub keeps its local
marker and retries with backoff until re-bound (documented edge case).

## Hub-side observability and logging

Tunnel metrics (session gauge, reconnects, expired leases, heartbeat RTT) are
exported through the OBS registry. Logging discipline (OBS S10): only node
ids and event names are ever logged — payload bytes, dial targets, proxy URLs
and secrets never appear.

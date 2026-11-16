# JikuNode

JikuNode is the supplier-side egress agent. It dials **out** to the hub
(`wss://<hub>/v1/node/tunnel`), so it works behind NAT, and dials upstream
hosts on the hub's behalf. It never holds a credential and never sees
plaintext: TLS to the upstream is terminated inside jikud.

Everything here runs as an unprivileged user. The only local secret is the
node's own ed25519 private key (mode `0600`).

## Install

Install a verified binary with the canonical per-user installer:

```sh
deploy/jikunode/install-user.sh /path/to/jikunode
```

It enables and starts the service when the systemd user manager is available;
otherwise it reports an error. Operation after logout may require an
administrator to run `loginctl enable-linger USER`.

The installer places the binary in `$HOME/.local/bin`, the unit in
`$XDG_CONFIG_HOME/systemd/user`, and the node key under
`$XDG_CONFIG_HOME/jikunode`. To remove it:

```sh
jikunode uninstall              # service and binary
jikunode uninstall --purge     # also node key/config/state/cache
```

The purge command does not delete the user's shared systemd journal.

For a system-wide installation, build from the repository root (Go 1.27):

```sh
go build -o jikunode ./cmd/jikunode
sudo install -m 0755 jikunode /usr/local/bin/jikunode
sudo useradd --system --home /var/lib/jikunode --shell /usr/sbin/nologin jikunode
```

Copy `jikunode.service` to `/etc/systemd/system/jikunode.service`, then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now jikunode
```

## Bind

Get your reusable 10-character binding code (`XXXXX-XXXXX`) from the provider page in the
Jiku console. It is reusable and does not expire. For the system-wide
service, bind as the service user so the key lands in the directory it reads:

```sh
sudo -u jikunode XDG_CONFIG_HOME=/var/lib/jikunode/.config \
  jikunode bind <code>
```

The service creates its identity and key offline, then idles without any network
connection until binding succeeds:

| path | mode | contents |
|---|---|---|
| `$XDG_CONFIG_HOME/jikunode/` | `0700` | config directory |
| `$XDG_CONFIG_HOME/jikunode/config.json` | `0600` | `hub_url`, `node_id`, `name`, `key_path` |
| `$XDG_CONFIG_HOME/jikunode/node.key` | `0600` | base64 of the 32-byte ed25519 seed |

Binding uses this existing identity and registers its public key with an HTTP
challenge and bind-proof-v2 signature; it opens no temporary tunnel. It writes
`config.json` and then the `bound` marker, which the running service picks up
without a restart. Writes are atomic (temp file + rename).
The offline UUID is stored in `node.id` (mode `0600`).

## Run

```sh
jikunode run            # connect, serve streams, reconnect with backoff
jikunode status         # loopback status JSON
jikunode pause          # ask the hub to pause this node
jikunode resume         # ask the hub to run this node
jikunode logs --n 50    # last log lines from the in-memory ring
jikunode version
```

The hub is authoritative for pause/resume: the stored state is pushed to the
node on every handshake and after every change, so `status.paused` is what the
hub says, never a local guess. `pause`/`resume` only send the request and wait
for that push.

Reconnects use jittered exponential backoff from 1 s to 60 s; the backoff
resets only after a session lasted more than 60 s.

## Local status page

The status server binds `127.0.0.1:16888` and **nothing else** — the code
refuses any other bind address. It serves exactly five routes:

```
GET  /                read-only status page (pause/resume buttons)
GET  /status          status JSON
GET  /logs?n=N        last N log lines
POST /control/pause   ask the hub to pause; it pushes desired_state back
POST /control/resume  ask the hub to run; it pushes desired_state back
```

Requests whose `Host` header is not loopback are refused (`403`, DNS-rebinding
defence) and POSTs require `Content-Type: application/json` or `X-Jikunode: 1`
(CSRF defence). No route accepts or displays a key, token or credential.

Inside Docker the page is reachable only from the container:

```sh
docker compose -f deploy/jikunode/compose.yml exec jikunode jikunode status
```

## Egress policy

After DNS resolution the node refuses private, loopback, link-local, CGNAT,
IPv6 ULA, unspecified and multicast destinations (`tunnel.DialPolicy`), then
dials the checked IP directly so the DNS answer cannot change between check and
dial. There is no host or port allowlist.

## Docker

```sh
docker compose -f deploy/jikunode/compose.yml up -d --build
docker compose -f deploy/jikunode/compose.yml exec jikunode jikunode \
  bind <code>
```

The image is a multi-stage build producing a static binary on
`distroless/static:nonroot`, with the config on a named volume and the
container read-only otherwise.


## Upgrades

User systemd installs support `jikunode upgrade [--version vX.Y.Z] [--check]`
and default-on hub-triggered signed upgrades. Set `auto_upgrade` to false in
config.json to decline hub requests. The user service uses Restart=always.
System units with ProtectSystem=strict report unsupported; an administrator
runs `sudo jikunode upgrade`. Docker uses new image tags; Android uses its
app store. Archives contain only jikunode; download install-user.sh separately.

## Known edge cases

Older or wire-incompatible nodes require manual reinstall because they cannot
complete the handshake to receive upgrade. A later crash loop needs manual
recovery using jikunode.prev; probation rollback is deferred. A single release
key is compiled in, so key rotation or compromise requires manual redistribution.
These limitations are acceptable during initial development.

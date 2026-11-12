# JikuNode

JikuNode is the supplier-side egress agent. It dials **out** to the hub
(`wss://<hub>/v1/node/tunnel`), so it works behind NAT, and dials upstream
hosts on the hub's behalf. It never holds a credential and never sees
plaintext: TLS to the upstream is terminated inside jikud.

Everything here runs as an unprivileged user. The only local secret is the
node's own ed25519 private key (mode `0600`).

## Install

The recommended release installer is a per-user installation and does not
require root:

```sh
curl -fsSL https://dev.jiku.app/node/install.sh | sh
```

After binding, enable the user service:

```sh
systemctl --user enable --now jikunode.service
```

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

Get your fixed six-character binding code from the provider page in the
Jiku console. It is reusable and does not expire. For the system-wide
service, bind as the service user so the key lands in the directory it reads:

```sh
sudo -u jikunode XDG_CONFIG_HOME=/var/lib/jikunode/.config \
  jikunode bind <code> --hub https://hub.jiku.app --name kitchen-pi
sudo systemctl restart jikunode
```

The command generates the keypair locally, registers only the public key and
writes, on success only:

| path | mode | contents |
|---|---|---|
| `$XDG_CONFIG_HOME/jikunode/` | `0700` | config directory |
| `$XDG_CONFIG_HOME/jikunode/config.json` | `0600` | `hub_url`, `node_id`, `key_path` |
| `$XDG_CONFIG_HOME/jikunode/node.key` | `0600` | base64 of the 32-byte ed25519 seed |

A failed bind writes nothing. Writes are atomic (temp file + rename).

## Run

```sh
jikunode run            # connect, serve streams, reconnect with backoff
jikunode status         # loopback status JSON
jikunode pause          # tell the hub to pause this node
jikunode resume         # tell the hub to run this node
jikunode logs --n 50    # last log lines from the in-memory ring
jikunode version
```

Reconnects use jittered exponential backoff from 1 s to 60 s; the backoff
resets only after a session lasted more than 60 s.

## Local status page

The status server binds `127.0.0.1:16888` and **nothing else** — the code
refuses any other bind address. It serves exactly five routes:

```
GET  /                read-only status page (pause/resume buttons)
GET  /status          status JSON
GET  /logs?n=N        last N log lines
POST /control/pause   send SetState(paused) to the hub
POST /control/resume  send SetState(running) to the hub
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
docker compose -f deploy/jikunode/compose.yml run --rm jikunode \
  bind <code> --hub https://hub.jiku.app --name <name>
```

The image is a multi-stage build producing a static binary on
`distroless/static:nonroot`, with the config on a named volume and the
container read-only otherwise.

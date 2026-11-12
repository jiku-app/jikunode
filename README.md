# JikuNode

JikuNode is the supplier-side Jiku egress agent. It dials out to a Jiku hub,
works behind NAT, and carries encrypted upstream traffic without holding
provider credentials or seeing plaintext requests.

## Install from a release

Download the archive for your platform from
[GitHub Releases](https://github.com/jiku-app/jikunode/releases), verify
`checksums.txt`, and install the `jikunode` binary. Linux systemd deployment
files are in [`deploy/jikunode`](deploy/jikunode).

The project currently publishes Linux `amd64` and `arm64` release artifacts.
The release workflow builds binaries with the tagged version and publishes a
SHA-256 checksum file.

## Bind and run

A binding code is issued once by the provider console:

```sh
jikunode bind <code> --hub https://hub.example.com --name kitchen-pi
jikunode run
```

Without `--hub`, `jikunode bind <code>` discovers the current hub from
`https://dev.jiku.app/api/v1/public/node-config`. Discovery sends no binding
code or private key, requires a valid HTTPS hub URL, and times out after
10 seconds. Use `--hub URL` to bypass discovery. Discovery failures are
reported rather than silently falling back to a stale address. The selected
hub is saved at binding time and reused for reconnects.
The bind command generates the ed25519 private key locally. Only the public
key is sent to the hub. Configuration and the private key are written with
restrictive permissions and are never printed.

Available commands:

```text
jikunode bind <code> [--hub URL] [--name NAME]
jikunode run
jikunode status
jikunode pause
jikunode resume
jikunode logs --n 50
jikunode uninstall              # remove the user service and binary
jikunode uninstall --purge     # also remove config/key/state/cache
jikunode version
```

## Protocol

The wire protocol is documented in [`docs/node-protocol.md`](docs/node-protocol.md)
and implemented in [`internal/tunnel`](internal/tunnel). The node uses a
WebSocket connection with yamux multiplexing, an ed25519 challenge-response
handshake, lease heartbeats, and a fail-closed post-DNS destination policy.

## systemd

```sh
go build -trimpath -o jikunode ./cmd/jikunode
sudo install -m 0755 jikunode /usr/local/bin/jikunode
sudo useradd --system --home /var/lib/jikunode --shell /usr/sbin/nologin jikunode
sudo install -m 0644 deploy/jikunode/jikunode.service /etc/systemd/system/jikunode.service
sudo systemctl daemon-reload
```

Bind as the service user, then start it:

```sh
sudo -u jikunode XDG_CONFIG_HOME=/var/lib/jikunode/.config \
  jikunode bind <code> --hub https://hub.example.com --name kitchen-pi
sudo systemctl enable --now jikunode
```

## Docker

The image is published to GHCR on every release:

```sh
docker pull ghcr.io/jiku-app/jikunode:latest
```

For the compose setup and binding instructions, see
[`deploy/jikunode/compose.yml`](deploy/jikunode/compose.yml).

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/jikunode
```

`uninstall` removes the per-user systemd unit and installed binary. Add
`--purge` to remove the node key, config, local state and cache. It does not
remove the user's shared systemd journal history.

The node repository contains no provider credentials, production database
code, or hosted-service secrets. Hub-side implementations remain in the
private hosted-service repository.

## License

Apache-2.0. See [`LICENSE`](LICENSE).

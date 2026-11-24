# JikuNode

JikuNode is the supplier-side Jiku egress agent. It dials out to a Jiku hub,
works behind NAT, and carries encrypted upstream traffic without holding
provider credentials or seeing plaintext requests.

This repository is the single source of truth for the node agent and wire
protocol. The hub and Android wrapper consume its public Go packages as a
pinned `github.com/jiku-app/jikunode` module; see [CONTRIBUTING.md](CONTRIBUTING.md)
for package boundaries, local workspaces and the protocol release workflow.

## Install from a release

Download the archive for your platform from
[GitHub Releases](https://github.com/jiku-app/jikunode/releases), verify
`checksums.txt`, and install the `jikunode` binary. Linux systemd deployment
files are in [`deploy/jikunode`](deploy/jikunode); Windows install files are in
[`deploy/windows`](deploy/windows).

The project publishes Linux and Windows `amd64` and `arm64` release artifacts.
The release workflow builds binaries with the tagged version and publishes a
SHA-256 checksum file signed with an ed25519 release key. Archives contain only
the regular file `jikunode`; download `install-user.sh` separately. Windows
archives (`jikunode_<version>_windows_<arch>.zip`) contain `jikunode.exe` and
`jikunode-tray.exe`; download `install-user.ps1` separately. The
`_portable.zip` variant adds the `jikunode.portable` marker file and needs no
installer.

## Bind and run

After verifying a release, `deploy/jikunode/install-user.sh /path/to/jikunode`
installs and starts the per-user service when a systemd user manager is available.
For operation after logout, an administrator may need `loginctl enable-linger USER`.
The service creates its identity offline and stays idle until bound.

A reusable 10-character binding code (`XXXXX-XXXXX`) is available from the provider console:

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
The node creates its identity and ed25519 private key locally. Only the public
key is sent to the hub. Configuration and the private key are written with
restrictive permissions and are never printed. An unbound service stays idle;
after binding, the running service connects automatically.

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
jikunode upgrade [--version vX.Y.Z] [--check]
jikunode version
```

## Windows

On Windows the node runs as a per-user startup app: `jikunode-tray.exe` starts
when you sign in, runs `jikunode.exe run` without a console window, and
restarts it if it exits (the job systemd does on Linux). No administrator
rights are needed. Install a verified release from PowerShell:

```powershell
.\install-user.ps1 .\jikunode_<version>_windows_amd64.zip -Checksums .\checksums.txt
```

Then type the binding code into the JikuNode window and press Bind (or run
`jikunode bind <code> --name office-pc`). For a portable copy that keeps its
data next to the programs (a USB drive, say), extract
`jikunode_<version>_windows_<arch>_portable.zip` anywhere and start
`jikunode-tray.exe`; nothing is installed.

The notification-area icon shows whether the node is connected, connecting,
paused, not bound or stopped. Click it (or start JikuNode from the Start menu)
for the JikuNode window: binding code entry, the node log, Start/Stop node, Pause/Resume,
the status page and logs, and two preferences, "Start JikuNode when I sign in to Windows" and
"Connect to the hub automatically when JikuNode starts". Right-click it for
the same actions as a menu. The CLI commands above all work in a terminal as
well. See [deploy/windows](deploy/windows/README.md) for details.

| | Windows location |
|---|---|
| Programs | `%LOCALAPPDATA%\Programs\jikunode` (added to your PATH) |
| Config and private key | `%APPDATA%\jikunode` (owner-only access list) |
| Logs and upgrade lock | `%LOCALAPPDATA%\jikunode` |
| Autostart | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, value `JikuNode` |

`XDG_CONFIG_HOME` and `XDG_STATE_HOME` still override the config and state
roots. If port 16888 is taken, install with `-Port <n>` and pass `--port <n>`
to `status`, `pause`, `resume` and `logs`. The node only runs while you are
signed in. See [`deploy/windows`](deploy/windows) for details.

## Protocol

The wire protocol is documented in [`docs/node-protocol.md`](docs/node-protocol.md)
and implemented in [`tunnel`](tunnel). The node uses a
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
go build -ldflags -H=windowsgui ./cmd/jikunode-tray   # Windows only
```

To build the Windows programs (both `.exe` files, a portable copy or an
installable zip) run `.\deploy\windows\build.ps1`; see
[Build from source](deploy/windows/README.md#build-from-source).

`uninstall` removes the per-user systemd unit and installed binary (on
Windows: the tray app's autostart entry, PATH entry and both programs). Add
`--purge` to remove the node key, config, local state and cache. It does not
remove the user's shared systemd journal history.

The node repository contains no provider credentials, production database
code, or hosted-service secrets. Hub-side implementations remain in the
private hosted-service repository.

## License

Apache-2.0. See [`LICENSE`](LICENSE).


## Signed upgrades

`jikunode upgrade` selects the latest GitHub release, or use
`--version vX.Y.Z` for an exact stable release. `--check` reports availability
without changing files. Only strictly newer stable versions are installed.
The node downloads from the fixed jiku-app/jikunode release repository,
verifies the ed25519 signature over `"jikunode-release-v1:" || checksums.txt`,
then verifies the archive SHA-256 and runs the binary's version smoke test.
Downloads are bounded (64 MiB archive, 64 KiB checksums, five-minute timeout);
redirects require HTTPS. The binary is replaced atomically and the old inode
is kept as `jikunode.prev`. An exclusive lock in
`$XDG_STATE_HOME/jikunode` (default `~/.local/state/jikunode`) serializes upgrades.
The CLI restarts an active user systemd service after installation.

On Windows the release asset is `jikunode_<version>_windows_<arch>.zip`, with the
same signature and checksum checks. A running `.exe` cannot be overwritten, so
each binary is renamed to `*.prev.exe` and the new one moved into place; the
lock lives in `%LOCALAPPDATA%\jikunode`. The CLI then asks the tray app to
restart, and the tray app also restarts itself when the node exits after a
hub-triggered upgrade.

After authentication, the hub can request an upgrade using only a version
tag. The CLI service supports this automatically; set `"auto_upgrade": false`
in `config.json` to decline (default true, effective on next connection).
Successful upgrades exit cleanly and `Restart=always` starts the new binary.
Failures preserve the session. A missing hook or non-writable binary directory
reports `unsupported`.

| Deployment | Upgrade |
|---|---|
| User systemd service in ~/.local/bin | Automatic and CLI |
| Windows tray app in %LOCALAPPDATA%\Programs\jikunode | Automatic and CLI |
| System unit with ProtectSystem=strict | Unsupported automatically; administrator runs sudo jikunode upgrade |
| Docker | Pull a new image tag |
| Android | App store |

## Known edge cases

- Wire-incompatible releases, nodes predating upgrade support, and nodes that
  cannot handshake need a manual reinstall. This is acceptable during initial
  development; future protocol rollout design can address it.
- A binary can pass its version check and crash-loop under `Restart=always`.
  `jikunode.prev` remains available for manual recovery; automatic probation
  and rollback require a later design.
- Releases trust a single compiled-in public key. Key rotation or compromise
  needs manual redistribution for now; a future design can introduce rotation.

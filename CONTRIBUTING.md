# Developing JikuNode

This repository (`github.com/jiku-app/jikunode`, Go 1.27) is the only source
of node-agent and node wire-protocol code. The `jiku.app` hub and Android
(gomobile) wrapper consume it as a pinned Go module, not an in-tree copy.
Hub implementation, storage, billing and hub-side integration tests live in
`jiku.app`. Do not copy node or protocol packages into that repository.

## Package boundary

| Path | Contents |
|---|---|
| `cmd/jikunode/` | CLI and loopback status server |
| `nodeagent/` | offline identity, config, HTTP bind, discovery, runner and hooks |
| `tunnel/` | wire messages, handshake, yamux sessions, heartbeat and destination policy |
| `bindcode/` | binding-code generation, normalization and checksum |
| `upgrade/` | signed release verification and atomic binary installation |
| `contract/` | shared RunState, TunnelMetrics and ErrNodeDialFailed |
| `deploy/jikunode/` | Dockerfile, compose, systemd unit and user installer |
| `docs/node-protocol.md` | canonical wire specification |

Public imports are `github.com/jiku-app/jikunode/{nodeagent,tunnel,bindcode,contract,upgrade}`.
Hub consumers must use the shared contract types and dial-error sentinel, not
redeclare them: Go type identity and `errors.Is` cross this module boundary.
Keep hub-side interfaces, secrets, credentials and database code out of this
public repository. The node never holds provider credentials or sees plaintext.

## Checks

```sh
go build ./... && go vet ./... && go test ./...
GOOS=linux GOARCH=arm64 go build ./cmd/jikunode
```

Tests use local fakes, never live hubs or providers. CI runs tests, vet and the
CLI build. Build output (`/jikunode`, `bin/`, `dist/`) is ignored.

## Local co-development

Place sibling checkouts at `../jikunode` and `../jiku.app`. In this repository,
create an untracked, gitignored workspace:

```sh
go work init . ../jiku.app
```

The resulting `go.work` contains `use` entries for `.` and `../jiku.app`.
Run node checks here and hub/mobile integration tests from the hub checkout
with `GOWORK=/absolute/path/to/jikunode/go.work`. This uses local node changes
without committing a `replace` directive or changing the hub's pinned version.
Both repositories must already import the public package paths. Use `GOWORK=off`
to test the released dependency independently. Never commit `go.work` or
`go.work.sum`.

## Protocol changes and releases

There is no version negotiation or compatibility layer. Every node/protocol
change is implemented and tested here, including an update to
`docs/node-protocol.md` when the wire changes. Then:

1. Run the checks above and the hub-side integration tests in `jiku.app` using
   the local workspace. Land the node change on `main` with CI green.
2. Publish a new annotated `vX.Y.Z` tag from this repository. Do not move or
   reuse published tags. Tagging/pushing requires an explicit maintainer action.
3. In `jiku.app`, run `go get github.com/jiku-app/jikunode@vX.Y.Z`, commit its
   module pin and run the hub/Android checks. Do not mirror source files.

The tag is the release version: `.github/workflows/release.yml` injects it via
`-ldflags "-X main.Version=<tag>"`. A pushed `v*.*.*` tag immediately publishes
Linux amd64/arm64 archives (one regular jikunode binary), checksums
signed checksums and a GitHub release (the user installer is a separate asset). The dependent container job builds
`deploy/jikunode/Dockerfile` and publishes GHCR tag and `latest` images. Release
only from `main`; the workflow does not enforce branch ancestry. Never publish
node releases from the hub repository.

## Known edge cases

A local workspace can hide a stale hub module pin. Disable the workspace for
the hub's release verification; no automatic pin synchronization is provided.


## Release signing

A dedicated ed25519 release key has already been provisioned. Its raw public
key is compiled into `upgrade.ReleasePublicKey` as base64. Before tagging any
release, install its PKCS#8 PEM private key in the repository's GitHub Actions
secret `RELEASE_SIGNING_KEY`. Never put a private key in the repository,
release assets, logs or command arguments.

For initial provisioning of a future dedicated key, an authorized maintainer
can use `openssl genpkey -algorithm ED25519 -out release.key.pem` in a private
directory with umask 077, then `openssl pkey -in release.key.pem -pubout -out
release.pub.pem`. Extract the raw 32-byte public key from its SubjectPublicKeyInfo
(the final 32 bytes of `openssl pkey -pubin -in release.pub.pem -outform DER`),
base64 encode it, and update the compiled constant before release. Store only
the private PEM in the Actions secret. Do not tag until the matching secret is
installed. Tests generate throwaway keys and inject the public key and local
HTTP server; they never use the release private key.

The workflow signs the exact bytes `jikunode-release-v1:` followed by
`checksums.txt`, using `openssl pkeyutl -sign -rawin`. It publishes the raw
64-byte signature as `checksums.txt.sig`. This domain separator must match the
node verifier. Both Linux archives contain exactly one regular file named
`jikunode`; the installer is published separately.

### Known edge cases

Release-key rotation/compromise requires manually redistributing a new trusted
binary; a rotation protocol is deferred. Wire-incompatible or pre-feature
nodes need manual reinstall. A post-smoke-test crash loop has no automatic
rollback; the retained `jikunode.prev` permits manual recovery until probation
rollback is designed.

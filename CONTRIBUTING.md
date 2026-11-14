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
| `contract/` | shared RunState, TunnelMetrics and ErrNodeDialFailed |
| `deploy/jikunode/` | Dockerfile, compose, systemd unit and user installer |
| `docs/node-protocol.md` | canonical wire specification |

Public imports are `github.com/jiku-app/jikunode/{nodeagent,tunnel,bindcode,contract}`.
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
Linux amd64/arm64 archives (binary, README, LICENSE and user installer), checksums
and a GitHub release. The dependent container job builds
`deploy/jikunode/Dockerfile` and publishes GHCR tag and `latest` images. Release
only from `main`; the workflow does not enforce branch ancestry. Never publish
node releases from the hub repository.

## Known edge cases

A local workspace can hide a stale hub module pin. Disable the workspace for
the hub's release verification; no automatic pin synchronization is provided.

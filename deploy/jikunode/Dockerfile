# JikuNode: static, non-root agent image.
#
# Build from the repository root so the module is complete:
#   docker build -f deploy/jikunode/Dockerfile -t jikunode:0.1.0 .

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY contract ./contract
COPY tunnel ./tunnel
COPY bindcode ./bindcode
COPY nodeagent ./nodeagent
COPY upgrade ./upgrade
ARG VERSION=0.1.0
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/jikunode ./cmd/jikunode

# distroless/static carries CA certificates (needed for wss) and runs as
# uid/gid 65532 (nonroot). There is no shell and no package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/jikunode /usr/local/bin/jikunode

# The config (hub URL, node id, key path) and the 0600 private key live here.
ENV XDG_CONFIG_HOME=/home/nonroot/.config
VOLUME ["/home/nonroot/.config"]

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/jikunode"]
CMD ["run"]

# The uspace-ussp Go image: the seven processes, each its own binary at
# /usr/bin/ussp-<process>; the compose service picks one as entrypoint.
# cmd/ussp-dev is development only and is not built here. Built in CI,
# never on a server.
#
#   docker build -f deploy/Dockerfile --build-arg VERSION=v0.1.0 --build-arg COMMIT=$(git rev-parse HEAD) .

FROM golang:1.27 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
# The embedded migration trees (the migrate subcommand; WP-1).
COPY migrations ./migrations
ARG VERSION=dev
ARG COMMIT=unknown
# No `go mod download`: it would fetch the tool directive's module
# (oapi-codegen) too; the build fetches only what the binaries import.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for p in api telemetry-ingest rid-sp monitor traffic-ws dss-sync tsdb-writer; do \
      go build -trimpath \
        -ldflags "-s -w -X github.com/rootxkit/uspace-ussp/internal/obs.Version=${VERSION} -X github.com/rootxkit/uspace-ussp/internal/obs.Commit=${COMMIT}" \
        -o "/out/ussp-${p}" "./cmd/${p}" || exit 1; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/bin/
USER nonroot:nonroot
EXPOSE 8080 8081 8082 8083 8084 8085 8086
ENTRYPOINT ["/usr/bin/ussp-api"]

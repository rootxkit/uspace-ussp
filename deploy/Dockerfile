# The uspace-ussp Go image: the seven processes, each its own binary at
# /usr/bin/ussp-<process>; the compose service picks one as entrypoint.
# cmd/ussp-dev is development only and is not built here. Built in CI,
# never on a server.
#
#   docker build -f deploy/Dockerfile --build-arg VERSION=v0.1.0 --build-arg COMMIT=$(git rev-parse HEAD) .
#
# The last stage is the production image (the default target). The
# `testif` target is the separate test-interfaces image
# (uspace-ussp-testif, docs/PLAN.md D13): cmd/test-interfaces alone, for
# the conformance profile; the production image never contains it.
#
#   docker build -f deploy/Dockerfile --target testif ... .

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

# The test-interfaces binary, built apart so that no production stage
# can copy it by accident.
FROM golang:1.27 AS build-testif
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath \
      -ldflags "-s -w -X github.com/rootxkit/uspace-ussp/internal/obs.Version=${VERSION} -X github.com/rootxkit/uspace-ussp/internal/obs.Commit=${COMMIT}" \
      -o /out/ussp-test-interfaces ./cmd/test-interfaces

FROM gcr.io/distroless/static-debian12:nonroot AS testif
COPY --from=build-testif /out/ussp-test-interfaces /usr/bin/ussp-test-interfaces
USER nonroot:nonroot
EXPOSE 8087
ENTRYPOINT ["/usr/bin/ussp-test-interfaces"]

FROM gcr.io/distroless/static-debian12:nonroot AS production
COPY --from=build /out/ /usr/bin/
USER nonroot:nonroot
EXPOSE 8080 8081 8082 8083 8084 8085 8086
ENTRYPOINT ["/usr/bin/ussp-api"]

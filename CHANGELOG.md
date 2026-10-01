# Changelog

All notable changes to `uspace-ussp`. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning, and the published API `api/openapi.yaml` changes
additively within `/v1`.

## [Unreleased]

### Added

- WP-0 scaffold: the Go module pinned to `uspace-core` v1.1.0; the seven
  processes (`api`, `telemetry-ingest`, `rid-sp`, `monitor`,
  `traffic-ws`, `dss-sync`, `tsdb-writer`) and `ussp-dev`, each serving
  `/healthz`, `/readyz` (every dependency with state, since, age and
  reason; 503 while a required one is down) and `/metrics`
  (`ussp_build_info`, `ussp_dependency_up`, `ussp_dependency_age_s`),
  starting degraded when PostgreSQL, TimescaleDB or NATS is down and
  draining on SIGTERM; the `USSP_*` configuration (`deploy/ENV.md`) with
  the token verifier configuration (`StrictSessionClaims` on); `httpx`
  (timeouts, body caps, request ids, recovery, problem+json, the scope
  middleware); the OpenAPI 3.1 skeleton with the generated server and
  client; the Makefile, CI, the two images and the compose stack; the
  web shell (Next.js, `ka`/`en`, generated API types).

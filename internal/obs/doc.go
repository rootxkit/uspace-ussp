// Package obs is the observability of every process: the slog JSON
// logger and its typed attribute helpers, the Prometheus registry with
// ussp_build_info and the dependency gauges, the OpenTelemetry tracer
// provider, the periodic status line, and the Health registry /readyz
// renders. Its output is written to be read by a supervisor at 3 a.m.:
// every dependency names its state, since when, how old the last good
// observation is, and why.
package obs

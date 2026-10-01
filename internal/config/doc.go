// Package config reads the configuration of every process from the
// environment (USSP_* variables, docs/PLAN.md §11) into one typed
// Config. Every variable is documented in deploy/ENV.md with the
// process that reads it, its default and its unit; a test keeps the two
// in step. Units are in the names (StatusIntervalS, MaxBodyBytes).
//
// A missing required variable is the only configuration error that
// stops a process (exit 2, the variable named); a dependency that is
// unreachable at start is reported on /readyz and never exits (B-08).
package config

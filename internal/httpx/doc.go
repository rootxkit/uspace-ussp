// Package httpx is the net/http baseline of every listener: header,
// read, write and idle timeouts; a header cap and a request body cap
// (1 MiB by default, per-route override); request ids; one structured
// access-log line per request; panic recovery into a problem+json 500
// (never a trace to the client); RFC 9457 problems in the shared shape;
// the bearer-token extraction and the scope middleware over a verifier
// interface; and a Shutdown bounded by a deadline.
package httpx

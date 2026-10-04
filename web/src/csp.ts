// The Content Security Policy of every page (the kit's, PLAN §8, M38):
// the API (through the BFF), the traffic and alert WebSockets and the
// basemap are same-origin (connect-src 'self'), fonts are self-hosted
// (font-src 'self'), MapLibre's workers come from blobs (worker-src
// blob:), there is no unsafe-eval in production and nothing is fetched
// from a third party. Scripts and the kit's injected styles (Radix
// ScrollArea) carry the per-request nonce. Development adds only what
// the dev server's hot reload needs.
export function contentSecurityPolicy(nonce: string, dev: boolean): string {
  return [
    "default-src 'self'",
    `script-src 'self' 'nonce-${nonce}' 'strict-dynamic'${dev ? " 'unsafe-eval'" : ""}`,
    dev ? "style-src 'self' 'unsafe-inline'" : `style-src 'self' 'nonce-${nonce}'`,
    "img-src 'self' data: blob:",
    "font-src 'self'",
    "connect-src 'self'",
    "worker-src blob:",
    "child-src blob:",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
  ].join("; ");
}

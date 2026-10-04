// The portal's Next.js configuration (uspace-ui docs/CONSUMING.md §11):
// standalone output for the image built in CI (deploy/web.Dockerfile;
// never built on a server), no transpilePackages (the kit ships compiled
// ESM), and the security headers every response carries. The Content
// Security Policy is set per request, with its nonce, in src/proxy.ts.
import type { NextConfig } from "next";

// A deployment's Caddy serves /basemap/* from the shared volume (M38) and
// this app serves no tiles. For a local run or the browser tests only,
// USSP_WEB_BASEMAP_ORIGIN at build time sends /basemap/* to a local
// server; the browser still sees one origin, so connect-src 'self' holds.
const basemapOrigin = process.env["USSP_WEB_BASEMAP_ORIGIN"] ?? "";

const config: NextConfig = {
  output: "standalone",
  poweredByHeader: false,
  reactStrictMode: true,
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "no-referrer" },
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=()" },
        ],
      },
    ];
  },
  async rewrites() {
    if (basemapOrigin === "") return [];
    return [{ source: "/basemap/:path*", destination: `${basemapOrigin.replace(/\/$/, "")}/basemap/:path*` }];
  },
};

export default config;

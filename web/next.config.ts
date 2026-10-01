import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // deploy/web.Dockerfile ships the standalone server, built in CI only.
  output: "standalone",
  poweredByHeader: false,
};

export default nextConfig;

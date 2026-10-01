// Serves the standalone build the way deploy/web.Dockerfile does: the
// static assets and public/ beside server.js (copied by
// `node e2e/serve.mjs --prepare`, which `pnpm test:e2e` runs first),
// then node runs server.js with PORT, HOSTNAME and USSP_WEB_API_URL
// from the environment.
import { cpSync, existsSync } from "node:fs";
import { spawn } from "node:child_process";

const root = new URL("../", import.meta.url);
const standalone = new URL(".next/standalone/", root);
if (!existsSync(new URL("server.js", standalone))) {
  console.error("no standalone build: run `pnpm build` first");
  process.exit(1);
}
if (process.argv.includes("--prepare")) {
  cpSync(new URL(".next/static/", root), new URL(".next/static/", standalone), { recursive: true });
  if (existsSync(new URL("public/", root))) {
    cpSync(new URL("public/", root), new URL("public/", standalone), { recursive: true });
  }
  process.exit(0);
}
const child = spawn(process.execPath, ["server.js"], { cwd: standalone, stdio: "inherit", env: process.env });
child.on("exit", (code) => process.exit(code ?? 1));
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => child.kill(sig));

// A stand-in for the api process in the smoke test: /readyz answers 503
// with a body in the contract's shape (nats down, the databases up),
// so the page's success path, a body it can read, is exercised too.
import { createServer } from "node:http";

const body = {
  status: "not_ready",
  checked_at: "2026-10-02T12:00:00Z",
  degraded: ["nats"],
  dependencies: {
    nats: { state: "down", required: true, since: "2026-10-02T11:59:00Z", age_s: 60, detail: "not connected (RECONNECTING)" },
    postgres: { state: "up", required: true, since: "2026-10-02T11:00:00Z", age_s: 0 },
    timescaledb: { state: "up", required: false, since: "2026-10-02T11:00:00Z", age_s: 0 },
  },
};
const port = Number(process.env.PORT ?? 3101);
createServer((req, res) => {
  if (req.url === "/readyz") {
    res.writeHead(503, { "Content-Type": "application/json" }).end(JSON.stringify(body));
  } else if (req.url === "/healthz") {
    res.writeHead(200, { "Content-Type": "application/json" }).end('{"status":"ok"}');
  } else {
    res.writeHead(404).end();
  }
}).listen(port, "127.0.0.1");

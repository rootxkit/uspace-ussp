// The console BFF's entries for the operating-status notices (Art. 7(6)):
// the exact path with GET and POST passes, every other method on it is
// a 405, and its neighbours are not let through (the kit's proxy answers
// 404 for a path no allow-list entry matches). Node's test runner on the
// pure functions of paths.ts (pnpm test:unit); the browser tests check
// the same answers at the running BFF.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { allowPaths, allowedMethods, consoleAllowPaths, consoleGetOnlyPaths, consoleMethodPaths, methodAllowed } from "./paths.ts";

const API = "http://api.test";
const STATUS = "/v1/admin/status";

/** Whether the console's allow-list lets upstream path through (the kit's proxy matches the whole upstream path). */
function consoleAllows(path: string): boolean {
  return consoleAllowPaths(API).some((re) => re.test(path));
}

/** The method decision of the console's proxy for path, as consoleProxy makes it. */
function consoleMethodAllowed(path: string, method: string): boolean {
  return methodAllowed(path, method, consoleGetOnlyPaths(), consoleMethodPaths());
}

describe("console BFF: operating-status notices", () => {
  it("lets the exact path through with GET and POST", () => {
    assert.equal(consoleAllows(STATUS), true);
    assert.equal(consoleMethodAllowed(STATUS, "GET"), true);
    assert.equal(consoleMethodAllowed(STATUS, "POST"), true);
    assert.deepEqual(allowedMethods(STATUS, consoleGetOnlyPaths(), consoleMethodPaths()), ["GET", "POST"]);
  });

  it("refuses every other method on it (405)", () => {
    for (const m of ["HEAD", "PUT", "PATCH", "DELETE", "OPTIONS", "get", "post"]) {
      assert.equal(consoleMethodAllowed(STATUS, m), false, m);
    }
  });

  it("refuses the path percent-encoded the same way", () => {
    assert.equal(consoleMethodAllowed("/v1/admin/%73tatus", "DELETE"), false);
    assert.equal(consoleMethodAllowed("/v1/admin/%73tatus", "POST"), true);
  });

  it("does not let its neighbours through (404)", () => {
    for (const p of [
      "/v1/admin/status/",
      "/v1/admin/status/1",
      "/v1/admin/statuses",
      "/v1/admin/statu",
      "/v1/admin/status.json",
      "/v1/admin/status?kind=start",
      "/v1/admin//status",
      "/v1/admin/x/status",
      "/v1/status",
      "/v2/admin/status",
      "/V1/admin/status",
    ]) {
      assert.equal(consoleAllows(p), false, p);
    }
  });

  it("leaves a neighbour's method to the allow-list, not to the status entry", () => {
    assert.equal(allowedMethods("/v1/admin/status/1", consoleGetOnlyPaths(), consoleMethodPaths()), null);
  });

  it("is not on the portal's BFF", () => {
    assert.equal(
      allowPaths(API).some((re) => re.test(STATUS)),
      false,
    );
  });
});

describe("console BFF: the GET-only summary keeps its methods", () => {
  it("takes GET and HEAD and nothing else", () => {
    const p = "/v1/admin/intents/summary";
    assert.equal(consoleMethodAllowed(p, "GET"), true);
    assert.equal(consoleMethodAllowed(p, "HEAD"), true);
    assert.equal(consoleMethodAllowed(p, "POST"), false);
  });

  it("leaves an unlisted method path to the API", () => {
    assert.equal(consoleMethodAllowed("/v1/admin/policy", "PUT"), true);
  });
});

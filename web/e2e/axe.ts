// axe-core in the page (the pinned axe-core devDependency, no network):
// WCAG 2.0, 2.1 and 2.2 A and AA rules, the accessibility target pending
// GCAA (config/portal.json). The violations as axe reports them.
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";

const require = createRequire(import.meta.url);
const source = readFileSync(require.resolve("axe-core/axe.min.js"), "utf8");

export interface Violation {
  id: string;
  impact: string | null;
  nodes: { target: string[] }[];
}

export default class AxeBuilder {
  constructor(private readonly page: Page) {}

  /** The violations, and how many rules passed (a run that checked nothing is not a pass). */
  async analyze(): Promise<{ violations: Violation[]; passes: number }> {
    // Through the debugging protocol, not a script element: the page's CSP
    // (nonce, strict-dynamic) stays as the portal sets it.
    await this.page.evaluate(source);
    return this.page.evaluate(async () => {
      const axe = (window as unknown as { axe: { run(ctx: Document, opts: object): Promise<{ violations: Violation[]; passes: unknown[] }> } }).axe;
      const r = await axe.run(document, { runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"] } });
      return { violations: r.violations.map((v) => ({ id: v.id, impact: v.impact, nodes: v.nodes.map((n) => ({ target: n.target })) })), passes: r.passes.length };
    });
  }
}

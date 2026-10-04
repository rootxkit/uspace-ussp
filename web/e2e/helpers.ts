import { expect, type APIRequestContext, type Page } from "@playwright/test";
import { CONTROL } from "../playwright.config";

/** A suffix that keeps this run's records apart from every other's. */
export function unique(): string {
  return `${Date.now() % 1_000_000_000}${Math.floor(Math.random() * 1000)}`;
}

/** The stack's control listener (test/e2e/stack). */
export async function control(request: APIRequestContext, path: string, body: Record<string, unknown>): Promise<Record<string, unknown>> {
  const res = await request.post(`${CONTROL}${path}`, { data: body });
  expect(res.ok(), `${path}: ${res.status()} ${await res.text()}`).toBe(true);
  return (await res.json()) as Record<string, unknown>;
}

/** The wall clock of a datetime-local box read as UTC (the kit's UTCDateTimeField). */
export function utcInput(d: Date): string {
  return d.toISOString().slice(0, 16);
}

/** English, by the language switch. */
export async function english(page: Page): Promise<void> {
  await page.context().addCookies([{ name: "uspace_lang", value: "en", url: page.url().startsWith("http") ? page.url() : "http://127.0.0.1:3200" }]);
}

/** Every request of the page goes to its own origin (M38: no third-party tile, font or script). */
export function sameOrigin(page: Page): () => string[] {
  const origins = new Set<string>();
  page.on("request", (req) => origins.add(new URL(req.url()).origin));
  return () => [...origins].filter((o) => o !== new URL(page.url()).origin && o !== "null");
}

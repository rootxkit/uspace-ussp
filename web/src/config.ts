// The portal's configuration: config/portal.json (display defaults) and
// the environment, read at request time on the server so the image is
// built once and configured at start (deploy/ENV.md, web). Nothing here
// is a judgement: every threshold on screen comes from an API answer.
import portal from "../config/portal.json";

export interface MapViewConfig {
  /** [lng, lat] of the maps' first view. */
  center: [number, number];
  zoom: number;
}

/** What the client components need, serialisable. */
export interface RuntimeConfig {
  mapView: MapViewConfig | null;
  /** Why mapView is null: the variable and what was wrong with it. */
  mapViewProblem: string | null;
  geoMarginFraction: number;
  accessibilityTarget: string;
  accessibilityStatus: string;
}

type Env = Record<string, string | undefined>;

function mapView(env: Env): { view: MapViewConfig | null; problem: string | null } {
  let center: [number, number] = [portal.map.center[0] ?? 0, portal.map.center[1] ?? 0];
  let zoom = portal.map.zoom;
  const rawCenter = env["USSP_WEB_MAP_CENTER"];
  if (rawCenter !== undefined && rawCenter !== "") {
    const parts = rawCenter.split(",").map((p) => Number(p.trim()));
    const [lng, lat] = parts;
    if (
      parts.length !== 2 ||
      lng === undefined ||
      lat === undefined ||
      !Number.isFinite(lng) ||
      !Number.isFinite(lat) ||
      lng < -180 ||
      lng > 180 ||
      lat < -90 ||
      lat > 90
    ) {
      return { view: null, problem: `USSP_WEB_MAP_CENTER: want lng,lat in degrees, got ${JSON.stringify(rawCenter)}` };
    }
    center = [lng, lat];
  }
  const rawZoom = env["USSP_WEB_MAP_ZOOM"];
  if (rawZoom !== undefined && rawZoom !== "") {
    const z = Number(rawZoom);
    if (!Number.isFinite(z) || z < 0 || z > 22) {
      return { view: null, problem: `USSP_WEB_MAP_ZOOM: want a number from 0 to 22, got ${JSON.stringify(rawZoom)}` };
    }
    zoom = z;
  }
  return { view: { center, zoom }, problem: null };
}

export function runtimeConfig(env: Env = process.env): RuntimeConfig {
  const m = mapView(env);
  return {
    mapView: m.view,
    mapViewProblem: m.problem,
    geoMarginFraction: portal.geo.margin_fraction,
    accessibilityTarget: portal.accessibility.target,
    accessibilityStatus: portal.accessibility.status,
  };
}

export interface BffEnv {
  /** The api process as this server reaches it (USSP_WEB_API_URL). */
  apiBase: string;
  /** False only for a plain-HTTP local run (USSP_WEB_SESSION_SECURE=false). */
  secure: boolean;
  /** Reverse proxies in front of Next.js (USSP_WEB_TRUSTED_PROXY_HOPS). */
  trustedProxyHops: number | null;
  timeoutMs: number;
  sessionMaxAgeS: number;
}

/**
 * The BFF's configuration, or the problem with the environment naming
 * the variable. A secure session needs the proxy hops said (the kit
 * refuses otherwise): behind the deployment's Caddy that is 1. The API
 * must list this container among USSP_TRUSTED_PROXIES, so its sign-in
 * limits key on the client the BFF names, not on the BFF.
 */
export function bffEnv(env: Env = process.env): { cfg: BffEnv } | { problem: string } {
  const apiBase = env["USSP_WEB_API_URL"] ?? "http://127.0.0.1:8080";
  if (!URL.canParse(apiBase)) {
    return { problem: `USSP_WEB_API_URL: not a URL: ${JSON.stringify(apiBase)}` };
  }
  const secure = env["USSP_WEB_SESSION_SECURE"] !== "false";
  let trustedProxyHops: number | null = null;
  const rawHops = env["USSP_WEB_TRUSTED_PROXY_HOPS"];
  if (rawHops !== undefined && rawHops !== "") {
    const n = Number(rawHops);
    if (!Number.isInteger(n) || n < 1) {
      return { problem: `USSP_WEB_TRUSTED_PROXY_HOPS: want a whole number of at least 1, got ${JSON.stringify(rawHops)}` };
    }
    trustedProxyHops = n;
  }
  if (secure && trustedProxyHops === null) {
    return {
      problem:
        "USSP_WEB_TRUSTED_PROXY_HOPS is not set (the reverse proxies in front of Next.js; USSP_WEB_SESSION_SECURE=false for a plain-HTTP local run)",
    };
  }
  let timeoutMs = portal.bff.timeout_ms;
  const rawTimeout = env["USSP_WEB_BFF_TIMEOUT_MS"];
  if (rawTimeout !== undefined && rawTimeout !== "") {
    const n = Number(rawTimeout);
    if (!Number.isInteger(n) || n < 100 || n > 120_000) {
      return { problem: `USSP_WEB_BFF_TIMEOUT_MS: want whole milliseconds from 100 to 120000, got ${JSON.stringify(rawTimeout)}` };
    }
    timeoutMs = n;
  }
  return { cfg: { apiBase, secure, trustedProxyHops, timeoutMs, sessionMaxAgeS: portal.bff.session_max_age_s } };
}

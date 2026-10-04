// The hand-written mapping from this USSP's stream bodies to the kit's
// view models (uspace-ui docs/CONSUMING.md §8). It copies what traffic-ws
// said and decides nothing: no age, no state, no identification, no
// distance, no threshold. A body that breaks its schema
// (schemas/traffic/product/v1, schemas/alert/v1) is refused here (null,
// counted by the caller), never repaired.
import { isAltSource, isAlertKind, isClearReason, isSeverity, isTrust, type AlertView, type Severity, type TrackView } from "@rootxkit/uspace-ui/model";
import type { ConsoleFrame } from "@rootxkit/uspace-ui/live";

export const PRODUCT_SCHEMA = "traffic/product/v1";
export const ALERT_SCHEMA = "alert/v1";
export const GEO_CHANGED_SCHEMA = "geo/changed/v1";

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);
const isStr = (v: unknown): v is string => typeof v === "string";
const isNum = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v);
const numOrNull = (v: unknown): number | null | undefined => (v === null ? null : isNum(v) ? v : undefined);

const TRACK_STATES = ["live", "stale", "source_disabled"] as const;
export type TrackState = (typeof TRACK_STATES)[number];
const IDENT = ["registered", "suspended", "unknown_operator", "unidentified"] as const;
type IdentStatus = (typeof IDENT)[number];

/** What the product says of a track beside the kit's view. */
export interface TrackFacts {
  state: TrackState;
  /** Seconds from the track's captured_at to the product's at (traffic-ws's clock). */
  ageS: number;
  own: boolean;
  peerUnavailable: boolean;
  callsign: string | null;
  /** The identification status only: the product carries no reason, serial or operator. */
  ident: IdentStatus | null;
}

export interface AdaptedTrack {
  view: Omit<TrackView, "receivedAtMs">;
  facts: TrackFacts;
}

export interface Degraded {
  input: string;
  since: string | null;
  reason: string;
}

export interface Product {
  at: string;
  intentId: string | null;
  tracks: AdaptedTrack[];
  /** Tracks of the product refused by the adapter (schema broken). */
  tracksRefused: number;
  alerts: PortalAlert[];
  alertsRefused: number;
  degraded: Degraded[];
  cisVersion: string | null;
  policyVersion: number;
  droppedFrames: number;
  throttled: boolean;
}

/** One product track as the kit's TrackView and the product's facts; null when it breaks the schema. */
export function adaptProductTrack(raw: unknown): AdaptedTrack | null {
  if (!isObj(raw)) return null;
  const { track_id, trust, source, state, age_s, position, alt_source, time_of_report } = raw;
  if (!isStr(track_id) || track_id === "" || !isTrust(trust) || !isStr(source) || source === "") return null;
  if (!TRACK_STATES.includes(state as TrackState) || !isNum(age_s) || age_s < 0 || !isStr(time_of_report)) return null;
  if (!isObj(position) || !isNum(position["lat"]) || !isNum(position["lng"]) || !isAltSource(alt_source)) return null;
  const altAmslM = numOrNull(raw["alt_amsl_m"]);
  const speedMs = numOrNull(raw["speed_ms"]);
  const trackDeg = numOrNull(raw["track_deg"]);
  const vspeedMs = numOrNull(raw["vspeed_ms"]);
  if (altAmslM === undefined || speedMs === undefined || trackDeg === undefined || vspeedMs === undefined) return null;
  const em = raw["emergency"];
  if (em !== null && typeof em !== "boolean") return null;
  const id = raw["identification"];
  let ident: IdentStatus | null = null;
  if (id !== null) {
    if (!isObj(id) || !IDENT.includes(id["status"] as IdentStatus)) return null;
    ident = id["status"] as IdentStatus;
  }
  const callsign = raw["callsign"];
  return {
    view: {
      trackId: track_id,
      trust,
      source,
      // The product names the source, not its instance.
      sourceInstance: "",
      lat: position["lat"],
      lng: position["lng"],
      altAmslM,
      altWgs84M: null,
      altSource: alt_source,
      heightM: null,
      heightRef: null,
      speedMs,
      trackDeg,
      vspeedMs,
      status: null,
      emergency: em === true,
      // The kit draws a track's identification from its status alone;
      // the product carries the status only (04 §3.2), so reason and
      // basis are not shown anywhere on this page (TrackFacts.ident is
      // what the list prints).
      identification: ident === null ? null : { status: ident, reason: "matched", serial: null, operatorReg: null, registeredOperatorReg: null, mismatch: false, basis: "authenticated" },
      flightId: null,
      intentId: null,
      times: { ts: time_of_report, rxTs: time_of_report, capturedAt: time_of_report, timeSource: "system", backlog: false },
    },
    facts: {
      state: state as TrackState,
      ageS: age_s,
      own: raw["own"] === true,
      peerUnavailable: raw["peer_unavailable"] === true,
      callsign: isStr(callsign) ? callsign : null,
      ident,
    },
  };
}

/**
 * An alert as the portal holds it: the alert/v1 body's members as sent,
 * plus the time this page received it. Its kind and clear reason stay
 * the wire's words (a kind the kit does not name, such as
 * identification, is shown as it came, never dropped or relabelled).
 */
export interface PortalAlert {
  alertId: string;
  kind: string;
  severity: Severity;
  state: "raised" | "updated" | "cleared";
  clearReason: string | null;
  flightId: string | null;
  intentId: string | null;
  authorisationNumber: string | null;
  peerTrackId: string | null;
  peerTrust: string | null;
  detail: Obj;
  capturedAt: string;
  raisedAt: string;
  updatedAt: string | null;
  policyVersion: number;
  ackedAt: string | null;
  escalatedAt: string | null;
}

/** An alert/v1 body; null when it breaks the schema. */
export function adaptAlertBody(b: unknown): PortalAlert | null {
  if (!isObj(b)) return null;
  const { alert_id, kind, severity, state, captured_at, raised_at, policy_version, detail } = b;
  if (!isStr(alert_id) || !isStr(kind) || !isSeverity(severity) || !isStr(captured_at) || !isStr(raised_at)) return null;
  if (state !== "raised" && state !== "updated" && state !== "cleared") return null;
  if (!isNum(policy_version) || !isObj(detail)) return null;
  const str = (k: string): string | null => (isStr(b[k]) ? (b[k] as string) : null);
  const peer = detail["peer"];
  return {
    alertId: alert_id,
    kind,
    severity,
    state,
    clearReason: str("clear_reason"),
    flightId: str("flight_id"),
    intentId: str("intent_id"),
    authorisationNumber: str("authorisation_number"),
    peerTrackId: isObj(peer) && isStr(peer["track_id"]) ? peer["track_id"] : null,
    peerTrust: isObj(peer) && isStr(peer["trust"]) ? peer["trust"] : null,
    detail,
    capturedAt: captured_at,
    raisedAt: raised_at,
    updatedAt: str("updated_at"),
    policyVersion: policy_version,
    ackedAt: str("acked_at"),
    escalatedAt: str("escalated_at"),
  };
}

/** The kit's view of an alert of a kind the kit names (for its summary line); null otherwise. */
export function kitAlertView(a: PortalAlert): AlertView | null {
  if (!isAlertKind(a.kind)) return null;
  return {
    alertId: a.alertId,
    kind: a.kind,
    severity: a.severity,
    state: a.state,
    clearReason: a.clearReason !== null && isClearReason(a.clearReason) ? a.clearReason : null,
    aircraft: a.flightId === null ? [] : [a.flightId],
    peerTrackId: a.peerTrackId,
    detail: a.detail,
    capturedAt: a.capturedAt,
    raisedAt: a.raisedAt,
    policyVersion: String(a.policyVersion),
    acknowledged: a.ackedAt !== null,
    receivedAtMs: 0,
  };
}



/** An alert/v1 frame's body; null when the frame is another schema or breaks it. */
export function adaptAlertFrame(f: ConsoleFrame): PortalAlert | null {
  return f.schema === ALERT_SCHEMA ? adaptAlertBody(f.body) : null;
}

/** A traffic/product/v1 frame; null when the body breaks the schema's required members. */
export function adaptProduct(f: ConsoleFrame): Product | null {
  if (f.schema !== PRODUCT_SCHEMA || !isObj(f.body)) return null;
  const b = f.body;
  const { at, tracks, alerts, degraded, policy_version, dropped_frames } = b;
  if (!isStr(at) || !Array.isArray(tracks) || !Array.isArray(alerts) || !Array.isArray(degraded)) return null;
  if (!isNum(policy_version) || !isNum(dropped_frames)) return null;
  const forObj = b["for"];
  const intentId = isObj(forObj) && isStr(forObj["intent_id"]) ? forObj["intent_id"] : null;
  const out: Product = {
    at,
    intentId,
    tracks: [],
    tracksRefused: 0,
    alerts: [],
    alertsRefused: 0,
    degraded: [],
    cisVersion: isStr(b["cis_version"]) ? b["cis_version"] : null,
    policyVersion: policy_version,
    droppedFrames: dropped_frames,
    throttled: b["throttled"] === true,
  };
  for (const raw of tracks) {
    const a = adaptProductTrack(raw);
    if (a === null) out.tracksRefused++;
    else out.tracks.push(a);
  }
  for (const raw of alerts) {
    const a = adaptAlertBody(raw);
    if (a === null) out.alertsRefused++;
    else out.alerts.push(a);
  }
  for (const d of degraded) {
    if (isObj(d) && isStr(d["input"]) && isStr(d["reason"]) && (d["since"] === null || isStr(d["since"]))) {
      out.degraded.push({ input: d["input"], since: d["since"] as string | null, reason: d["reason"] });
    }
  }
  return out;
}

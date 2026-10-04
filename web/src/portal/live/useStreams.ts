"use client";

// The intent's live streams on this origin (brief WP-17; M22, M29): the
// kit's live client on WS /v1/traffic?intent_id= and WS
// /v1/alerts?intent_id=, the session cookie on the same-origin upgrade,
// never a ticket and never a token in the URL. Status frames fill the
// feed status (connection, dropped_frames, degraded, policy version,
// stale_after_s); every other frame comes here and goes through the
// adapter. A 4401 close sends the person to sign in again.
import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { createTrackStore, useFeed, useStore, type ConsoleFrame, type LiveFeed } from "@rootxkit/uspace-ui/live";
import type { TrackView } from "@rootxkit/uspace-ui/model";
import { ALERT_SCHEMA, GEO_CHANGED_SCHEMA, PRODUCT_SCHEMA, adaptAlertFrame, adaptProduct, type PortalAlert, type Product, type TrackFacts } from "./adapt";

/** Trail points kept per track. A display bound. */
export const TRAIL_POINTS = 20;
/** Tracks this page holds; past it the least recently updated is evicted by the kit and counted. A display bound. */
export const MAX_TRACKS = 2000;
/** Alerts this page holds; past it the oldest cleared one goes first. A display bound (E-10). */
export const MAX_ALERTS = 500;

/** What the page counted and did not show, by cause: nothing is dropped silently. */
export interface StreamCounters {
  productsRefused: number;
  tracksRefused: number;
  alertsRefused: number;
  framesNotShown: number;
  alertsEvicted: number;
}

const ZERO: StreamCounters = { productsRefused: 0, tracksRefused: 0, alertsRefused: 0, framesNotShown: 0, alertsEvicted: 0 };

export interface HeldAlert extends PortalAlert {
  /** How many times this alert's frames arrived (the stream repeats an unacknowledged critical alert). */
  received: number;
  receivedAtMs: number;
}

/** The alerts of a stream, keyed by id, bounded. */
function useAlertMap(): [ReadonlyMap<string, HeldAlert>, (a: PortalAlert) => void, number] {
  const [alerts, setAlerts] = useState<ReadonlyMap<string, HeldAlert>>(new Map());
  const [evicted, setEvicted] = useState(0);
  const apply = useCallback((a: PortalAlert) => {
    setAlerts((prev) => {
      const next = new Map(prev);
      const old = next.get(a.alertId);
      next.set(a.alertId, { ...a, received: (old?.received ?? 0) + 1, receivedAtMs: Date.now() });
      if (next.size > MAX_ALERTS) {
        const victim = [...next.values()].sort((x, y) => Number(x.state !== "cleared") - Number(y.state !== "cleared") || x.receivedAtMs - y.receivedAtMs)[0];
        if (victim !== undefined) {
          next.delete(victim.alertId);
          setEvicted((n) => n + 1);
        }
      }
      return next;
    });
  }, []);
  return [alerts, apply, evicted];
}

export interface Traffic {
  feed: LiveFeed;
  product: Product | null;
  tracks: ReadonlyMap<string, TrackView>;
  facts: ReadonlyMap<string, TrackFacts>;
  /** The stream's alert/v1 frames (proximity alerts of the intent as they come). */
  alerts: ReadonlyMap<string, HeldAlert>;
  counters: StreamCounters;
  geoChanges: number;
}

function useLogin(): () => void {
  const router = useRouter();
  return useCallback(() => router.replace("/login"), [router]);
}

/** WS /v1/traffic for one of the operator's intents. */
export function useTraffic(intentId: string, onGeoChanged?: () => void): Traffic {
  const toLogin = useLogin();
  const [store] = useState(() => createTrackStore({ trailPoints: TRAIL_POINTS, maxTracks: MAX_TRACKS }));
  const [product, setProduct] = useState<Product | null>(null);
  const [facts, setFacts] = useState<ReadonlyMap<string, TrackFacts>>(new Map());
  const [counters, setCounters] = useState<StreamCounters>(ZERO);
  const [alerts, applyAlert, evicted] = useAlertMap();
  const [geoChanges, setGeoChanges] = useState(0);
  const geoRef = useRef(onGeoChanged);
  useEffect(() => {
    geoRef.current = onGeoChanged;
  }, [onGeoChanged]);
  const bump = (k: keyof StreamCounters, n = 1) => setCounters((c) => ({ ...c, [k]: c[k] + n }));
  const onFrame = (f: ConsoleFrame) => {
    if (f.schema === PRODUCT_SCHEMA) {
      const p = adaptProduct(f);
      if (p === null) {
        bump("productsRefused");
        return;
      }
      if (p.tracksRefused > 0) bump("tracksRefused", p.tracksRefused);
      if (p.alertsRefused > 0) bump("alertsRefused", p.alertsRefused);
      // A product is the whole picture of the second: the tracks it holds
      // replace the page's; one that leaves a track out takes it off (the
      // product keeps a silent track with its age, B-11; a throttled
      // product carries only the tracks due, so it updates and keeps).
      if (p.throttled) for (const t of p.tracks) store.upsert(t.view);
      else store.replace(p.tracks.map((t) => t.view));
      setFacts((prev) => {
        const next = p.throttled ? new Map(prev) : new Map<string, TrackFacts>();
        for (const t of p.tracks) next.set(t.view.trackId, t.facts);
        return next;
      });
      setProduct(p);
    } else if (f.schema === ALERT_SCHEMA) {
      const a = adaptAlertFrame(f);
      if (a === null) bump("alertsRefused");
      else applyAlert(a);
    } else if (f.schema === GEO_CHANGED_SCHEMA) {
      setGeoChanges((n) => n + 1);
      geoRef.current?.();
    } else {
      bump("framesNotShown");
    }
  };
  const feed = useFeed({ url: `/v1/traffic?intent_id=${encodeURIComponent(intentId)}`, onUnauthorized: toLogin, onFrame });
  const tracks = useStore(store);
  return { feed, product, tracks, facts, alerts, counters: { ...counters, alertsEvicted: evicted }, geoChanges };
}

export interface AlertStream {
  feed: LiveFeed;
  alerts: ReadonlyMap<string, HeldAlert>;
  counters: StreamCounters;
}

/** WS /v1/alerts for one of the operator's intents. */
export function useAlertStream(intentId: string): AlertStream {
  const toLogin = useLogin();
  const [counters, setCounters] = useState<StreamCounters>(ZERO);
  const [alerts, applyAlert, evicted] = useAlertMap();
  const feed = useFeed({
    url: `/v1/alerts?intent_id=${encodeURIComponent(intentId)}`,
    onUnauthorized: toLogin,
    onFrame(f) {
      const a = adaptAlertFrame(f);
      if (a !== null) applyAlert(a);
      else setCounters((c) => (f.schema === ALERT_SCHEMA ? { ...c, alertsRefused: c.alertsRefused + 1 } : { ...c, framesNotShown: c.framesNotShown + 1 }));
    },
  });
  return { feed, alerts, counters: { ...counters, alertsEvicted: evicted } };
}

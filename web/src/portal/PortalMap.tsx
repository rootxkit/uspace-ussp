"use client";

// The kit's MapView with the portal's first view (configuration, never a
// constant in code) and the basemap from this origin's /basemap/,
// served by the deployment (M38): no third-party tile or font request.
// Without a valid configured view the map is not drawn and says why.
import type { ReactNode } from "react";
import { useEffect, useSyncExternalStore } from "react";
import { useLang } from "@rootxkit/uspace-ui/i18n";
import { MapView, subscriptionBBox, useBBoxSubscription, useMapContext, type BBox, type Viewport } from "@rootxkit/uspace-ui/map";
import { useTheme } from "@rootxkit/uspace-ui/theme";
import { useRuntimeConfig } from "@/components/Providers";
import { useAppT } from "@/i18n/t";

function noSubscribe(): () => void {
  return () => undefined;
}

export function PortalMap({
  children,
  onViewport,
  className = "h-[420px]",
}: {
  children?: ReactNode;
  onViewport?(v: Viewport, b: BBox): void;
  className?: string;
}) {
  const t = useAppT();
  const { lang } = useLang();
  const { resolved } = useTheme();
  const cfg = useRuntimeConfig();
  // The origin is known in the browser only; the map renders there.
  const origin = useSyncExternalStore(
    noSubscribe,
    () => window.location.origin,
    () => null,
  );
  if (cfg.mapView === null) {
    return (
      <p role="alert" data-testid="map-not-configured">
        {t("portal.map.not_configured", { problem: cfg.mapViewProblem ?? "" })}
      </p>
    );
  }
  return (
    <div className={`relative ${className}`} data-testid="map-host">
      {origin !== null && (
        <MapView
          className="absolute inset-0"
          basemap={{ baseUrl: origin }}
          initial={{ center: cfg.mapView.center, zoom: cfg.mapView.zoom, bearing: 0, pitch: 0 }}
          lang={lang}
          scheme={resolved}
          {...(onViewport === undefined ? {} : { onViewport })}
        >
          {children}
        </MapView>
      )}
    </div>
  );
}

/** How long the map must rest before its box is asked for. Display-only. */
export const VIEW_DEBOUNCE_MS = 500;
/** The box is quantised to this grid so a small pan asks nothing new. Display-only. */
export const QUANTIZE_DEG = 0.01;

/** west,south,east,north as the API's bbox parameter takes it. */
export function bboxParam(b: BBox): string {
  return [b.minLng, b.minLat, b.maxLng, b.maxLat].join(",");
}

/**
 * Calls onChange with the padded, quantised box of the enclosing map:
 * the configured first view's at once (also when the browser draws no
 * map, so the answer still arrives in the list), then after every
 * settled move (the kit's hook).
 */
export function ViewBox({ margin, onChange }: { margin: number; onChange(b: BBox): void }) {
  const { map, initialBBox } = useMapContext();
  useEffect(() => {
    if (map === null) onChange(subscriptionBBox(initialBBox, margin, QUANTIZE_DEG));
  }, [map, initialBBox, margin, onChange]);
  useBBoxSubscription({ marginFraction: margin, quantizeDeg: QUANTIZE_DEG, debounceMs: VIEW_DEBOUNCE_MS, onChange });
  return null;
}


"use client";

// The portal's translate function, typed by its catalogue: a key that
// is not in en.json fails `tsc`. Kit keys go through the kit's own t.
import { useCallback } from "react";
import { useT, type Vars } from "@rootxkit/uspace-ui/i18n";
import type { AppKey } from "./catalogues";

export type AppT = (key: AppKey, vars?: Vars) => string;

export function useAppT(): AppT {
  const t = useT();
  return useCallback((key: AppKey, vars?: Vars) => t(key, vars), [t]);
}

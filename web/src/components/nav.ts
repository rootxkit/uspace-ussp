// The navigation of both consoles' frames as data: groups of entries,
// each with its catalogue key, icon and the roles that use it, and which
// entry a path is. Hiding an entry grants and refuses nothing: the API
// decides every request.
import type { ReactNode } from "react";
import type { NavGroup } from "@rootxkit/uspace-ui/ui";
import type { AppKey } from "@/i18n/catalogues";

export interface NavEntry {
  href: string;
  key: AppKey;
  icon: ReactNode;
  /** Shown only to these roles; to every role when absent. */
  roles?: string[];
  /** Also current on this path's children (the intents list for an intent). */
  prefix?: boolean;
}

export interface NavSection {
  id: string;
  /** The group's heading; a group without one runs on from the last. */
  key?: AppKey;
  items: NavEntry[];
}

/** The entry the path is: its own, else the longest prefix entry it is under. */
export function currentEntry(nav: readonly NavSection[], path: string): NavEntry | undefined {
  const all = nav.flatMap((g) => g.items);
  return (
    all.find((n) => n.href === path) ??
    all.filter((n) => n.prefix === true && path.startsWith(`${n.href}/`)).sort((a, b) => b.href.length - a.href.length)[0]
  );
}

/** The kit's navigation groups for the roles: an entry a role does not use is left out. */
export function navGroups(nav: readonly NavSection[], current: NavEntry | undefined, roles: readonly string[], t: (k: AppKey) => string): NavGroup[] {
  return nav
    .map((g) => ({
      id: g.id,
      ...(g.key === undefined ? {} : { label: t(g.key) }),
      items: g.items
        .filter((n) => n.roles === undefined || n.roles.some((r) => roles.includes(r)))
        .map((n) => ({ id: n.href, label: t(n.key), href: n.href, icon: n.icon, active: n === current })),
    }))
    .filter((g) => g.items.length > 0);
}

// The English catalogue. Every key here must exist in ka.ts, and the
// reverse: ka.ts is typed by this file, so a missing or extra key fails
// `tsc` and the build.
export const en = {
  "app.title": "uspace USSP",
  "app.subtitle": "U-space Service Provider",
  "lang.en": "English",
  "lang.ka": "ქართული",
  "readiness.heading": "Service readiness",
  "readiness.source": "Readiness of the API process, read at {time}",
  "readiness.unreachable": "The API did not answer: {error}",
  "readiness.unreachable.hint": "Nothing below is known while the API is unreachable.",
  "readiness.status.ready": "Ready",
  "readiness.status.degraded": "Ready, degraded",
  "readiness.status.not_ready": "Not ready",
  "readiness.column.dependency": "Dependency",
  "readiness.column.state": "State",
  "readiness.column.required": "Required",
  "readiness.column.since": "Since",
  "readiness.column.age": "Last seen up",
  "readiness.column.detail": "Detail",
  "readiness.state.up": "Up",
  "readiness.state.degraded": "Degraded",
  "readiness.state.down": "Down",
  "readiness.state.unknown": "Unknown",
  "readiness.required.yes": "Yes",
  "readiness.required.no": "No",
  "readiness.age.seconds": "{seconds} s ago",
  "readiness.age.never": "Never",
} as const;

export type MessageKey = keyof typeof en;
export type Catalogue = Record<MessageKey, string>;

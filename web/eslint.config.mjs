// The kit's ESLint config, as every web/ extends it (uspace-ui
// docs/CONSUMING.md §4): no geometry or geodesy import, no database or
// bus client, no business logic in a route handler (app/%5Fbff/** imports
// only the kit's BFF helpers, next/* and lib/bff/*), no hand-written type
// in src/api/generated/. This repo adds its own list (CLAUDE.md, spec 06
// T12: a Next.js file importing geometry fails lint) on top.
import kit from "@rootxkit/uspace-ui/eslint";

const forbiddenImports = {
  paths: [
    { name: "turf", message: "web/ renders only: geometry is judged in uspace-core, never in the browser." },
    { name: "proj4", message: "web/ renders only: geodesy is uspace-core's." },
    { name: "geolib", message: "web/ renders only: geometry is judged in uspace-core, never in the browser." },
    { name: "h3-js", message: "web/ renders only; cells are uspace-core geodesy/cell and never leave the backend." },
    { name: "cheap-ruler", message: "web/ renders only: distances are judged in uspace-core." },
    { name: "pg", message: "web/ has no database: it reads the API through the BFF." },
    { name: "nats", message: "web/ has no NATS client: live data comes over the traffic WebSocket." },
    { name: "nats.ws", message: "web/ has no NATS client: live data comes over the traffic WebSocket." },
  ],
  patterns: [
    { group: ["@turf/*"], message: "web/ renders only: geometry is judged in uspace-core, never in the browser." },
    { group: ["pg-*", "@nats-io/*"], message: "web/ has no database or NATS client." },
  ],
};

export default [
  {
    ignores: [".next/**", "node_modules/**", "next-env.d.ts", "playwright-report/**", "test-results/**"],
  },
  ...kit,
  {
    rules: {
      "no-restricted-imports": ["error", forbiddenImports],
    },
  },
];

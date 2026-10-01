import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

// web/ renders only (CLAUDE.md engineering rules): no geometry library
// (a second judgement in TypeScript is a review failure, rule 3), no
// database or NATS client, and no hand-written API type.
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

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
  {
    rules: {
      "no-restricted-imports": ["error", forbiddenImports],
    },
  },
  {
    // src/api/ holds only the generated types.ts; a hand-written type
    // there would drift from api/openapi.yaml.
    files: ["src/api/**/*.ts", "src/api/**/*.tsx"],
    ignores: ["src/api/types.ts"],
    rules: {
      "no-restricted-syntax": [
        "error",
        { selector: "TSTypeAliasDeclaration", message: "API types are generated from api/openapi.yaml (pnpm gen:api), never written by hand." },
        { selector: "TSInterfaceDeclaration", message: "API types are generated from api/openapi.yaml (pnpm gen:api), never written by hand." },
        { selector: "TSEnumDeclaration", message: "API types are generated from api/openapi.yaml (pnpm gen:api), never written by hand." },
      ],
    },
  },
  globalIgnores([
    ".next/**",
    "out/**",
    "build/**",
    "next-env.d.ts",
    "playwright-report/**",
    "test-results/**",
    "src/api/types.ts",
  ]),
]);

export default eslintConfig;

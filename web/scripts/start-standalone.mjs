import { cpSync } from "node:fs";

// `output: standalone` intentionally excludes static assets from its traced
// server bundle. The Dockerfile copies these directories into place; doing the
// same here keeps `npm start` behavior identical to the production image.
cpSync(".next/static", ".next/standalone/.next/static", { recursive: true });
cpSync("public", ".next/standalone/public", { recursive: true });

await import("../.next/standalone/server.js");

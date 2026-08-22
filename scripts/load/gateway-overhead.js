// What does the gateway itself cost?
//
// This is the number that decides whether a gateway is worth deploying at all,
// and it is easy to measure dishonestly. Sending load through the gateway and
// reporting p95 measures the *model*, not the gateway: llama3.2:3b takes ~450ms
// to answer, so any proxy in front of it looks fast.
//
// So this test measures the same work twice — once straight at Ollama, once
// through Conduit — under identical concurrency, and reports the difference.
// That difference is the overhead, and it is the only honest version of the
// claim.
//
//   k6 run scripts/load/gateway-overhead.js
//
// Env: GATEWAY_URL, OLLAMA_URL, MODEL, API_KEY (optional)

import http from "k6/http";
import { check } from "k6";
import { Trend, Rate } from "k6/metrics";

const GATEWAY = __ENV.GATEWAY_URL || "http://localhost:8080";
const OLLAMA = __ENV.OLLAMA_URL || "http://localhost:11434";
const MODEL = __ENV.MODEL || "llama3.2:3b";
const API_KEY = __ENV.API_KEY || "";

const direct = new Trend("latency_direct_ms", true);
const viaGateway = new Trend("latency_gateway_ms", true);
const directFailures = new Rate("failures_direct");
const gatewayFailures = new Rate("failures_gateway");

export const options = {
  scenarios: {
    // One scenario, alternating paths inside each iteration — not two scenarios
    // run back to back.
    //
    // Two scenarios seemed cleaner and gave a wrong answer: whatever drifts
    // between the first block and the second (GPU clocks, page cache, the
    // model's KV cache) lands entirely in whichever path ran second, and gets
    // reported as gateway overhead. Interleaving makes both paths share the
    // same conditions request for request, so the difference is the gateway
    // and nothing else.
    paired: {
      executor: "constant-vus",
      exec: "comparePair",
      vus: 4,
      duration: "90s",
    },
  },

  // k6 reports med/p(90)/p(95) by default; p50 and p99 have to be asked for.
  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],

  thresholds: {
    failures_direct: ["rate<0.01"],
    failures_gateway: ["rate<0.01"],

    // The gateway must not add meaningful latency. An absolute bound rather
    // than a percentage of total: a percentage would quietly pass on a slow
    // model and fail on a fast one, when the work being measured is the same
    // fixed amount either way.
    latency_gateway_ms: ["p(95)<3000"],
  },
};

// Temperature 0.9 and a nonce keep the cache out of the measurement. A cache
// hit answers in ~2ms, which would make the gateway look thousands of times
// faster than the model — true, but not what this test is asking.
function body(nonce) {
  return JSON.stringify({
    model: MODEL,
    messages: [{ role: "user", content: `Reply with one short sentence. [${nonce}]` }],
    max_tokens: 24,
    temperature: 0.9,
  });
}

function headers(withAuth) {
  const h = { "Content-Type": "application/json" };
  if (withAuth && API_KEY) h["Authorization"] = `Bearer ${API_KEY}`;
  return h;
}

// One iteration = one request down each path, with the order alternating so
// that neither path is systematically the "warm" one.
export function comparePair() {
  const gatewayFirst = __ITER % 2 === 0;

  if (gatewayFirst) {
    hitGateway();
    hitOllamaDirectly();
  } else {
    hitOllamaDirectly();
    hitGateway();
  }
}

function hitOllamaDirectly() {
  const nonce = `${__VU}-${__ITER}-d`;
  const res = http.post(`${OLLAMA}/v1/chat/completions`, body(nonce), {
    headers: headers(false),
    timeout: "120s",
    tags: { path: "direct" },
  });

  direct.add(res.timings.duration);
  const ok = check(res, { "direct: 200": (r) => r.status === 200 }, { path: "direct" });
  directFailures.add(!ok);
}

function hitGateway() {
  const nonce = `${__VU}-${__ITER}-g`;
  const res = http.post(`${GATEWAY}/v1/chat/completions`, body(nonce), {
    headers: headers(true),
    timeout: "120s",
    tags: { path: "gateway" },
  });

  viaGateway.add(res.timings.duration);
  const ok = check(
    res,
    {
      "gateway: 200": (r) => r.status === 200,
      "gateway: names the provider": (r) => !!r.headers["X-Conduit-Provider"],
    },
    { path: "gateway" },
  );
  gatewayFailures.add(!ok);
}

export function handleSummary(data) {
  const d = data.metrics.latency_direct_ms?.values ?? {};
  const g = data.metrics.latency_gateway_ms?.values ?? {};

  const rows = [
    ["", "direct", "gateway", "overhead"],
    ["med", d.med, g.med, g.med - d.med],
    ["p95", d["p(95)"], g["p(95)"], g["p(95)"] - d["p(95)"]],
    ["p99", d["p(99)"], g["p(99)"], g["p(99)"] - d["p(99)"]],
    ["avg", d.avg, g.avg, g.avg - d.avg],
    ["min", d.min, g.min, g.min - d.min],
  ];

  const fmt = (v) => (typeof v === "number" ? `${v.toFixed(1)}ms` : String(v));
  const text = rows
    .map((r) => r.map((c, i) => (i === 0 ? String(c).padEnd(6) : fmt(c).padStart(12))).join(""))
    .join("\n");

  return {
    stdout: `\n  Gateway overhead — same work, measured both ways\n\n${text}\n\n`,
    "load-overhead.json": JSON.stringify(data, null, 2),
  };
}

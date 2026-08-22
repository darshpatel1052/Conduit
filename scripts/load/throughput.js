// How much traffic can the gateway itself carry?
//
// Run against the mock provider (scripts/load/mockprovider), so the model is
// out of the experiment and what is measured is the gateway: routing, the
// middleware chain, connection pooling, allocation per request. Against a real
// model this test would report the model's ceiling and nothing else.
//
//   go run ./scripts/load/mockprovider -addr :19100 &
//   ./bin/conduit -config scripts/load/conduit.load.yaml &
//   k6 run scripts/load/throughput.js
//
// Env: GATEWAY_URL, MODEL, API_KEY, STREAM_RATIO (0..1)

import http from "k6/http";
import { check } from "k6";
import { Trend, Rate, Counter } from "k6/metrics";

const GATEWAY = __ENV.GATEWAY_URL || "http://localhost:8080";
const MODEL = __ENV.MODEL || "mock-model";
const API_KEY = __ENV.API_KEY || "";
const STREAM_RATIO = Number(__ENV.STREAM_RATIO ?? 0.3);

// The rate to ramp to. Raise it until errors appear or latency climbs — the
// point where that happens is the gateway's actual ceiling, and it is the only
// version of a throughput number worth quoting. A run that completes cleanly
// proves the gateway carried the offered load, not that it could not carry
// more.
const PEAK_RATE = Number(__ENV.PEAK_RATE ?? 3000);

const blocking = new Trend("blocking_ms", true);
const streaming = new Trend("streaming_ms", true);
const errors = new Rate("errors");
const served = new Counter("served");

export const options = {
  scenarios: {
    // Ramping arrival rate, not ramping VUs. With VUs, a gateway that slows
    // down receives *less* traffic — each VU waits longer before sending
    // again — so the load backs off exactly when it should be pressing, and
    // saturation never shows up. Arrival rate keeps the offered load fixed and
    // lets latency and errors reveal the ceiling.
    ramp: {
      executor: "ramping-arrival-rate",
      startRate: 100,
      timeUnit: "1s",
      preAllocatedVUs: 200,
      maxVUs: 4000,
      stages: [
        { target: Math.round(PEAK_RATE / 6), duration: "20s" },
        { target: Math.round(PEAK_RATE / 2), duration: "30s" },
        { target: PEAK_RATE, duration: "30s" },
        { target: PEAK_RATE, duration: "30s" },
        { target: 0, duration: "10s" },
      ],
    },
  },

  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],

  thresholds: {
    errors: ["rate<0.01"],
    blocking_ms: ["p(95)<250", "p(99)<500"],
    streaming_ms: ["p(95)<500"],
  },
};

function headers() {
  const h = { "Content-Type": "application/json" };
  if (API_KEY) h["Authorization"] = `Bearer ${API_KEY}`;
  return h;
}

export default function () {
  // A unique prompt per request. Repeating one would be served from the cache
  // after the first hit, which measures the cache rather than the gateway —
  // a separate and much easier question.
  const stream = Math.random() < STREAM_RATIO;
  const payload = JSON.stringify({
    model: MODEL,
    messages: [{ role: "user", content: `request ${__VU}-${__ITER}-${Date.now()}` }],
    max_tokens: 64,
    temperature: 0.9,
    stream,
  });

  const res = http.post(`${GATEWAY}/v1/chat/completions`, payload, {
    headers: headers(),
    timeout: "30s",
  });

  const ok = check(res, { "200": (r) => r.status === 200 });
  errors.add(!ok);
  if (ok) served.add(1);

  (stream ? streaming : blocking).add(res.timings.duration);
}

export function handleSummary(data) {
  const m = data.metrics;
  const reqs = m.http_reqs?.values ?? {};
  const b = m.blocking_ms?.values ?? {};
  const s = m.streaming_ms?.values ?? {};
  const err = m.errors?.values?.rate ?? 0;

  const line = (label, v) =>
    `  ${label.padEnd(22)}${typeof v === "number" ? v.toFixed(1) : v}`;

  return {
    stdout: [
      "",
      "  Gateway throughput (mock upstream — the model is not in this measurement)",
      "",
      line("requests/s", reqs.rate),
      line("total requests", reqs.count),
      line("error rate %", err * 100),
      "",
      "  blocking:",
      line("  med ms", b.med),
      line("  p95 ms", b["p(95)"]),
      line("  p99 ms", b["p(99)"]),
      "",
      "  streaming (full response):",
      line("  med ms", s.med),
      line("  p95 ms", s["p(95)"]),
      "",
    ].join("\n"),
    "load-throughput.json": JSON.stringify(data, null, 2),
  };
}

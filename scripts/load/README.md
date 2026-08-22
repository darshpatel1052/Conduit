# Load tests

Two questions, two tests, because they need opposite setups.

## 1. What does the gateway cost? — `gateway-overhead.js`

Run against **real Ollama**. Sends the same work down two paths — straight at
Ollama, and through Conduit — and reports the difference.

```bash
./bin/conduit -config configs/conduit.yaml &
k6 run scripts/load/gateway-overhead.js
```

Measured on llama3.2:3b, RTX 4060, 4 concurrent, 90s, ~230 pairs:

| | direct | gateway | overhead |
|---|---|---|---|
| median | 925.4ms | 935.2ms | **+9.8ms** |
| p95 | 1077.0ms | 1087.2ms | **+10.2ms** |
| p99 | 1117.7ms | 1227.1ms | +109.4ms |
| min | 545.5ms | 528.2ms | **−17.3ms** |

The negative minimum is the important row. The gateway cannot make a request
faster than the upstream it forwards to, so −17.3ms is noise — which puts the
measurement's resolution in the same range as the thing being measured. Read
the median honestly as "about 10ms, and the error bars are not much smaller
than that". The p99 gap is real but is tail behaviour (GC, scheduler), not a
per-request cost.

**The first version of this test reported +175ms and was wrong.** It ran the two
paths as separate scenarios, 35 seconds apart. Everything that drifts between
two blocks of time — GPU clocks, the model's KV cache, thermal state — landed
entirely in whichever path ran second and got reported as gateway overhead.
Interleaving the two paths inside one iteration made the number fall by 94%.
The lesson generalises: when a benchmark compares A and B, anything that
separates them in time is a confound, and the confound is usually larger than
the effect.

## 2. How much traffic can it carry? — `throughput.js`

Run against the **mock provider**, so the model is out of the experiment. This
matters: llama3.2:3b answers in ~450ms and saturates one GPU at a handful of
concurrent requests, so a throughput test pointed at it reports the model's
ceiling no matter how the gateway is written.

```bash
go run ./scripts/load/mockprovider -addr :19100 &
./bin/conduit -config scripts/load/conduit.load.yaml &
PEAK_RATE=15000 k6 run scripts/load/throughput.js
```

At 15,000 req/s offered for 30s at plateau, 30% streaming, 1,038,372 requests:

| | |
|---|---|
| sustained | **~15,000 req/s** |
| errors | **0** |
| blocking median / p95 / p99 | 1.3ms / 5.9ms / **10.4ms** |
| streaming median / p95 | 2.6ms / 8.3ms |
| k6 dropped iterations | 127 (0.012%) |

**This is a floor, not a ceiling.** The completed request count matched the
offered load, latency stayed in single-digit milliseconds and nothing failed —
the gateway was never saturated. What ran out first was the test rig: k6, the
mock upstream and the gateway all shared one machine's CPUs and one loopback
interface. The honest claim is "carried 15k req/s without strain", not "peaks
at 15k req/s". Finding the real knee needs the load generator on separate
hardware.

Two configuration details do the heavy lifting, and both are in
`conduit.load.yaml`:

- `max_idle_conns_per_host: 1024`. Go's default is **2**. Past two concurrent
  requests to one provider, every additional request pays a fresh TCP handshake
  and then discards the connection. This is the single biggest thing a naive
  gateway gets wrong, and it does not show up until you load it.
- `log.level: error`. At thousands of requests per second, per-request info
  logging is the gateway writing about itself rather than doing its job.
  Production runs at info; a measurement of the request path should not.

## Why `ramping-arrival-rate` and not VUs

With a fixed pool of virtual users, a gateway that slows down receives *less*
traffic — each VU waits longer before sending again. The offered load backs off
exactly when it should be pressing, and saturation never appears. Arrival rate
holds the offered load fixed and lets latency and errors reveal the ceiling,
which is the entire point.

## Loading the failure paths

The mock provider can misbehave on purpose, which is how the breaker and
failover paths get exercised under load rather than only in tests:

```bash
go run ./scripts/load/mockprovider -addr :19100 -fail-rate 0.3 -latency 50ms
```

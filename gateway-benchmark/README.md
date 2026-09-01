# AI Gateway Benchmark

A runnable comparison of AI gateways: what putting one in your request path
costs, and how each one behaves when things go wrong.

Measures **Everstack, LiteLLM, Portkey Gateway, Bifrost, and Helicone AI
Gateway** on latency, plus **OpenRouter and Cloudflare AI Gateway** on
capabilities.

Written by Everstack, which is one of the gateways being measured. The
[methodology](METHODOLOGY.md) explains what we did about that; the short version
is that the report computes its own "where we lose" section, and you can re-run
everything yourself.

## The idea in three lines

1. Every gateway proxies to the **same deterministic mock upstream**, so the
   variance of a real model provider cannot be mistaken for a gateway difference.
2. The harness also talks to that upstream **directly**, and every published
   latency figure is the delta against that control.
3. The mock can be told to **misbehave on cue**, which is what makes failover,
   retry, timeout, and cache-isolation testable rather than assertable.

## Requirements

- Go 1.25+
- Docker with Compose v2 (only for the competitor gateways; the harness and mock
  upstream run without it)

On Apple Silicon, note that Helicone's image is amd64-only and runs under
emulation. Its latency and CPU figures are **not comparable** to the natively
running targets; the harness marks it and the report says so.

## Quick start

```sh
git clone https://github.com/everstacklabs/examples
cd examples/gateway-benchmark

make up          # mock upstream + every public competitor gateway
make validate    # confirm each one actually answers
make bench       # full suite, writes results/latest.md
make down
```

No Docker, or just want to see the harness work:

```sh
make upstream                          # terminal 1
go run ./cmd/gwbench run -only direct  # terminal 2
```

That runs the control path end to end and writes a real report. It is the
fastest way to confirm a new machine is set up correctly.

## Running the Everstack column

Everstack's *container images* are private, but its **server binary is published
on the public releases page**, so `compose/everstack/Dockerfile` builds the
subject from that. No registry credentials are needed.

It is behind a Compose profile because it also needs Postgres, Redis,
ClickHouse, and an OTLP collector, which is a lot to ask of a laptop that is
already running four competitor gateways:

```sh
make up-subject     # builds Everstack, starts its deps, mints its key, validates
make bench
```

`make up-subject` runs `make bootstrap` for you. That script exists because a
fresh Everstack cannot serve an authenticated request until a key row is in its
database, and there is no CLI command to mint one. It also disables the shipped
500 rpm rate limit, which is **not** special-casing: no other gateway here has a
limit active during the perf phases, and the rate-limit scenario (C5) tests
limits deliberately and separately. See the comments in the script.

**Give this room.** The subject's five containers plus four competitors plus the
load generator did not fit on an 18 GB laptop: ClickHouse was OOM-killed
mid-run, and the resulting numbers were unusable for Everstack *and* for Bifrost
measured alongside it. If the machine is tight, measure the competitors and the
subject in separate runs (`-only everstack`), each against its own control.

## What it measures

**Performance (P1 to P7)** - added latency p50/p95/p99, streaming TTFT, streaming
inter-chunk cadence, sustained throughput, behaviour across a concurrency sweep,
CPU-seconds and RSS per 10k requests, and overhead while the upstream is
returning 503 for one request in five.

**Behaviour (C1 to C12)** - failover, Retry-After compliance, retry
amplification, mid-stream failure handling, rate-limit shape, budget
enforcement, cache correctness, parameter fidelity, token accounting,
cross-tenant cache isolation, timeout ownership, and observability depth.

Every C-verdict is derived from the mock's request journal, so "it retried four
times" is a counted fact rather than an inference from client-side timing.

**Capability matrix** - `matrix/capabilities.yaml`. Every scored cell carries an
evidence link and a source. `make matrix` refuses to load a matrix with an
uncited claim.

## Why you can trust the numbers

These are enforced by the code and the tests, not by good intentions:

- **Open-loop load generation.** A closed-loop generator stops offering load
  exactly when the system slows down, so its tail latency is far better than a
  real client population would see. Coordinated omission is the single most
  common reason a published gateway benchmark is wrong.
- **Latency is measured from the scheduled arrival**, not from when a worker
  picked the request up.
- **Only successful responses enter the distribution.** A fast 500 is not a fast
  response, and letting errors into the histogram is how a failing gateway posts
  the best p99 in the table.
- **Unique prompt per request** in the performance phases, so an incidentally
  enabled cache cannot turn a proxy benchmark into a cache benchmark.
- **The report generates its own "Where we lose" section** from the same data as
  every other table, and says so explicitly when the subject drops out. Shipping
  a flattering report requires deleting code.
- **`unknown` costs nothing in the matrix.** A vendor researched less does not
  score worse for it; coverage is reported separately so thin homework is visible.
- **Targets that fail preflight are named in the report as unmeasured**, never
  silently dropped.

## Scope: two tiers of competitor

**Tier 1, in the latency tables** (self-hostable, so overhead can be isolated):
Everstack, LiteLLM, Portkey Gateway, Bifrost, Helicone AI Gateway, plus the
`direct` control.

**Tier 2, capability matrix only** (cloud-only): OpenRouter, Cloudflare AI
Gateway. Measuring a cloud gateway from a laptop measures the internet.
Publishing such a number is the most common way these comparisons get torn
apart, so this harness declines to produce one.

## Credentials

Keys are read from the environment, never from `targets.yaml`:

```sh
export EVERSTACK_BENCH_KEY=...
export EVERSTACK_BENCH_KEY_TENANT_B=...   # enables the tenant-isolation scenario
export LITELLM_BENCH_KEY=...
```

The mock upstream ignores a key's value and stores only a fingerprint, so its
request journal can tell two callers apart without ever holding a credential.
A test asserts this.

## Layout

```
cmd/mockupstream/   deterministic upstream, fault injection, request journal
cmd/gwbench/        load generator, scenario runner, matrix and report tooling
internal/upstream/  mock server and scenario control API
internal/loadgen/   open-loop generator, streaming timing capture
internal/stats/     exact quantiles, run aggregation
internal/harness/   target definitions, performance phases, C1-C12 scenarios
internal/matrix/    capability matrix model, validation, scoring
internal/report/    markdown and JSON generation
compose/            pinned docker-compose stack and every gateway's config
matrix/             the capability matrix data
results/            run output (gitignored)
```

## What is actually verified

Being precise about this matters more here than anywhere else, since the whole
point of the harness is that it does not let inference pass as measurement.

**Run-verified** - the harness itself and the `direct` control. The mock
upstream's pacing reproduces exactly (302 ms unary against a 300 ms configured
delay, 133 ms TTFT against 120 ms, 13.3 ms inter-chunk against 12 ms, and
exactly 20.0% injected errors at a 0.2 fault rate). The correctness scenarios
are proven non-vacuous by tests: C7 flips from `not configured` to `pass` when a
real caching proxy is put in front of the upstream, and C1 reports `fail` when a
fallback is claimed but not delivered.

**Container-verified** (2026-08-31, Docker 29.4 / OrbStack, Apple Silicon) - the
LiteLLM, Bifrost, Helicone, and Portkey configs under `compose/`. All four start,
proxy to the mock, and pass `make validate`. Getting there took a fix per
gateway, each recorded as a comment next to the setting it explains:

| Gateway | What was wrong |
| --- | --- |
| LiteLLM | pinned tag did not exist; harness key default did not match the configured master key |
| Bifrost | config mounted at the wrong path; SSRF guard blocks private IPs; base URL double-prefixed `/v1` |
| Helicone | config path is a CLI flag, not an env var; `load-balance` takes `providers` not `targets`; model names must be `{provider}/{model}`; serves `/ai/chat/completions`; drops path segments from a base URL |
| Portkey | custom host needs the `/v1` suffix or its 404 is forwarded as if the upstream rejected the request |

**Still not run-verified** - the **Everstack** target. Its image is private, so
the subject under test is the one config here that has never started. `make
validate` reports it as failing and a run lists it as unmeasured.

**Documented, not verified** - the `source: docs` cells in the capability
matrix. Their evidence URLs resolve, but the claims come from published vendor
documentation rather than a system anyone here drove.

**Weakest of all: `source: repo`** - Everstack's own cells cite a source tree
that is not public. Do not take them on faith; run the harness. Scenarios C1 to
C11 probe most of those claims directly, and a probe verdict overrides a
citation.

## Corrections welcome

If a competitor is misconfigured here, that is a bug and we want the issue. The
configs are committed precisely so they can be argued with. Same for a matrix
cell that is wrong or out of date: `make links` catches citations that have
moved, but only a human catches a citation that still resolves and no longer
says what it used to.

## Before publishing a run

1. Fill in `hardware:` in `targets.yaml`. A benchmark without the machine it ran
   on is not reproducible.
2. `make validate` and confirm no target is unexpectedly failing.
3. Read the "Where we lose" section first. If it is empty, that is a reason for
   suspicion rather than satisfaction: check that every competitor was
   configured for production rather than defaults, and that the load actually
   reached each one's saturation point.
4. Re-verify the matrix: run `make links` to catch citations that have moved,
   then confirm each surviving URL still **supports the cell it is attached to**.
   Update `verified_on` only after doing this, not as a formality.
5. Publish `results/latest.json` alongside the report. The raw data is the point.

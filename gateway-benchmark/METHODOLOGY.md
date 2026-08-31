# Methodology

The reasoning behind every measurement decision in this harness. If you disagree
with something here, that is the useful kind of disagreement: open an issue, and
if you are right the numbers change.

---

## 1. Why this exists

An AI gateway sits in the request path of every model call an application makes.
Two questions follow immediately, and neither is answerable from a vendor's
marketing page:

1. What does putting a gateway in that path cost, in latency and in compute?
2. What does that cost buy that a bare HTTP proxy would not?

This benchmark answers both with published numbers, committed configs, and raw
per-request data anyone can re-run. It is written by Everstack, which is one of
the gateways being measured. Section 2 is about what we did to stop that from
making the results worthless.

## 2. The credibility constraint

A vendor benchmark where the vendor wins every axis is worthless. Nobody
believes it, and rightly so.

So the design rule is: **optimise for being hard to refute, not for winning.**

Concretely:

- Every gateway is pinned to a specific released version, recorded in the output.
- Every config we run is committed in-repo, not described in prose.
- Raw per-request samples are published alongside the summary, not just percentiles.
- Load generation is open-loop, so coordinated omission does not flatter tails.
- We publish the axes where we lose, with a one-line explanation of why.

If Everstack loses raw proxy overhead to Bifrost, the report says so, because
the report computes that section rather than being handed it. Our claim is not
"fastest proxy"; it is that the overhead is small and bounded, and that what it
buys is visible in the behavioural scenarios. Neither half of that is worth
anything if the overhead number is not trusted.

## 3. Scope: two tiers of competitor

Latency overhead can only be isolated when the gateway runs on the same host as
the harness and points at the same upstream. That splits the field.

**Tier 1 - in the latency harness (self-hostable, docker-runnable):**

| Gateway | Image | Why it is in |
| --- | --- | --- |
| Everstack | private image, see README | subject under test |
| LiteLLM Proxy | `ghcr.io/berriai/litellm` | the OSS default, biggest mindshare |
| Bifrost | `maximhq/bifrost` | markets itself on raw proxy speed, so it is the hardest overhead comparison |
| Helicone AI Gateway | `helicone/ai-gateway` | Rust router, observability-adjacent positioning |
| Portkey Gateway | `portkeyai/gateway` | OSS core of a widely deployed commercial gateway |
| *direct* | none | control baseline: harness straight to upstream |

**Tier 2 - feature matrix only (cloud-only, cannot be overhead-isolated):**

OpenRouter, Cloudflare AI Gateway, Portkey Cloud, Helicone Cloud, AWS Bedrock
gateway patterns, Azure AI Foundry.

We deliberately do **not** publish latency numbers for tier 2. Measuring a cloud
gateway from a laptop measures the internet, not the gateway. Saying so in the
methodology is itself a credibility signal, because it is the most common way
these comparisons are faked.

## 4. Measurement architecture

```
                    ┌──────────────────────────────────┐
                    │  gwbench (open-loop load gen)    │
                    │  Poisson arrivals @ target RPS   │
                    └───────────────┬──────────────────┘
                                    │  identical OpenAI-format requests
            ┌───────────────────────┼───────────────────────┐
            │                       │                       │
            ▼                       ▼                       ▼
     ┌────────────┐          ┌────────────┐          ┌────────────┐
     │  direct    │          │ everstack  │          │  litellm   │   ... etc
     │ (control)  │          │ /openai/v1 │          │            │
     └─────┬──────┘          └─────┬──────┘          └─────┬──────┘
           │                       │                       │
           └───────────────────────┼───────────────────────┘
                                   ▼
                    ┌──────────────────────────────────┐
                    │  mockupstream                    │
                    │  deterministic TTFT, chunk pace, │
                    │  token count, fault injection    │
                    └──────────────────────────────────┘
```

**The mock upstream is the load-bearing design decision.** A real provider
varies by hundreds of milliseconds run to run. Gateway overhead is sub-millisecond
to low-milliseconds. Benchmarking through a live provider means measuring
OpenAI's variance and calling it a gateway difference. The mock removes that:

- fixed TTFT (default 120 ms) and fixed inter-chunk delay (default 12 ms)
- deterministic token stream, so token accounting is checkable
- programmable faults: status codes, latency spikes, connection hangs, partial
  streams - which is what makes failover and retry scenarios measurable at all
- it records every request it received, so we can verify what the gateway
  actually forwarded (retry counts, header passthrough, parameter fidelity)

`direct` (harness straight to the mock, no gateway) is the control. Every
latency number in the report is expressed as **added latency over control**, not
as an absolute, because the absolute is an artifact of the mock's configuration.

A second, clearly-labelled mode (`--live`) runs the same scenarios against a real
provider for end-to-end realism. Its numbers are reported separately and never
mixed with the overhead numbers.

## 5. What the benchmark measures

### 5.1 Performance dimensions

| # | Dimension | Metric | Why a buyer cares |
| --- | --- | --- | --- |
| P1 | Non-streaming added latency | p50 / p95 / p99 / max delta vs control | the headline "what does the hop cost" |
| P2 | Streaming TTFT overhead | delta on time-to-first-token | perceived latency in chat UIs |
| P3 | Streaming cadence overhead | inter-chunk p50/p99, jitter, stall count | gateways that buffer instead of stream show up here |
| P4 | Throughput / saturation | sustained RPS before p99 added latency exceeds 50 ms | capacity per replica |
| P5 | Concurrency scaling | added latency at 1/10/50/200/500 in-flight | connection-pool and event-loop behaviour |
| P6 | Resource cost | container CPU-seconds and peak RSS per 10k requests | what the hop costs in infra spend |
| P7 | Failure-mode overhead | added latency while upstream is 20% erroring | most gateways are only benchmarked healthy |

### 5.2 Correctness and behaviour scenarios

Speed is table stakes. These are the scenarios that separate a proxy from a
control plane, and every one is pass/fail with evidence from the mock's request log.

| # | Scenario | What we assert |
| --- | --- | --- |
| C1 | Failover | primary returns 503; measure time-to-recovery and whether the client ever sees an error |
| C2 | Retry semantics | 429 with `Retry-After`; assert the gateway honours it rather than hot-looping |
| C3 | Retry amplification | count upstream requests per client request under fault; a gateway that fans out 5x is a cost bug |
| C4 | Streaming failover | upstream dies mid-stream after N chunks; does the gateway recover, truncate, or hang |
| C5 | Rate limit accuracy | configure 60 rpm, drive 600 rpm, measure allowed count and error shape |
| C6 | Budget enforcement | spend cap set below run cost; assert requests are refused at the cap |
| C7 | Cache correctness | exact-match and semantic cache hit rate, hit latency, and that a cache hit is not served across tenants |
| C8 | Parameter fidelity | tools, tool_choice, response_format, logprobs, seed, stop, n survive the round trip unmangled |
| C9 | Token accounting | reported usage matches the mock's deterministic ground truth |
| C10 | Tenant isolation | key A cannot read key B's traffic, cache entries, or logs |
| C11 | Timeout behaviour | upstream hangs; assert the gateway times out rather than holding the client connection |
| C12 | Observability completeness | after a run, how much of the request is actually retrievable: prompt, completion, tokens, cost, latency, tool calls, errors, trace linkage |

C10 and C12 are where the Everstack argument lives, and C3 and C11 are where
free proxies most often fail quietly.

### 5.3 Feature matrix

A declarative matrix in `matrix/` scores each vendor across
capability groups. Each cell carries an evidence URL and a verification date, so
the matrix is auditable rather than assertive. Cells are one of:

`yes` / `partial` / `no` / `paid` (gated behind a commercial tier) / `unknown`

Groups: routing and reliability, caching, governance and cost control, security
and tenancy, observability depth, agent runtime, deployment model, extensibility,
protocol surface (OpenAI / Anthropic / Gemini / OTLP / MCP / A2A).

## 6. Reporting rules

1. Report added latency, not absolute latency, for tier 1.
2. Publish sample counts, run counts, warmup discarded, and hardware.
3. Publish the standard deviation across runs, not just the best run.
4. Any axis Everstack loses is in the report with a one-line reason.
5. The feature matrix is separate from the performance table. Never blend a
   feature win into a latency chart.
6. Every vendor config is committed. The report links to it.

## 7. Layout

```
gateway-benchmark/
  cmd/mockupstream/   deterministic OpenAI-compatible upstream + fault injection
  cmd/gwbench/        load generator, scenario runner, matrix and report tooling
  internal/upstream/  mock server, request journal, scenario control
  internal/loadgen/   open-loop generator, streaming timing capture
  internal/stats/     exact quantiles, run aggregation
  internal/harness/   target definitions, scenario definitions
  internal/matrix/    feature matrix model and scoring
  internal/report/    markdown and JSON report generation
  compose/            pinned docker-compose stack for every tier-1 gateway
  matrix/             the feature matrix data (YAML, evidence-linked)
  results/            run output (gitignored except .gitkeep)
```

## 8. How to run

```sh
make up          # start the mock upstream and every tier-1 gateway
make validate    # confirm each target actually answers
make bench       # run the full suite
make report      # regenerate the markdown report
make down
```

## 9. Open risks

- **Everstack does OpenAI to proto-JSON transforms in both directions** on
  `/openai/v1`. That is real work a pure byte-forwarding proxy does not do, and
  it may cost us on P1. If the overhead turns out to be large rather than small,
  that is a performance bug this benchmark found, which is a good outcome.
- **Competitor configs must be fair.** Running LiteLLM without its recommended
  production settings (multiple workers, no debug logging, no Redis hop it does
  not need) would be a rigged comparison. Every compose file carries a comment
  justifying each setting. If one of them is wrong for your product, that is a
  bug report we want.
- **Single-host benchmarking** hides distributed behaviour. Stated as a limit.
- **The subject's image is not public.** Everstack's container is in a private
  registry, so reproducing the Everstack column needs credentials. Every other
  column, including the control, reproduces from public images alone.

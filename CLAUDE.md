# bo-service-kit

The shared Go module every service imports. `telemetry/`, `chaos/`, `httpx/`.

Canonical spec: [`bo-platform/docs/BUILD-PLAN.md`](https://github.com/bo-jr/bo-platform/blob/main/docs/BUILD-PLAN.md)
§4 (layout), Phase 2 (the services).

## Why this repo exists

The three services must behave **identically** in everything except their business
logic — same metric names and labels, same trace propagation, same log shape, same
shutdown semantics, same chaos knobs. If that behaviour is copy-pasted three times it
diverges within a week, and every Phase 6 scenario becomes untrustworthy: you can no
longer tell whether a difference between services is real or an artifact of drift.

## Packages

| Package | Holds |
|---|---|
| `telemetry/` | OTel setup, OTLP export to local Alloy, W3C traceparent propagation, structured JSON logging with `trace_id` |
| `chaos/` | `FAILURE_RATE`, `EXTRA_LATENCY_MS`, `CPU_BURN_FACTOR`, `APP_VERSION` — read at startup, stamped into a `version` label |
| `httpx/` | server scaffolding: `/healthz`, `/readyz`, `/metrics`, graceful SIGTERM shutdown with connection draining |

`http_requests_total{service,route,status,version}` and
`http_request_duration_seconds` (same labels) are defined **here**, once. A service that
registers its own copy is a bug.

## Get the chaos interface right early, then leave it alone

This is the code you will iterate on most, and across a polyrepo split every change
otherwise becomes: tag the kit, `go get -u` in three repos, three PRs.

**Use `go.work`.** A workspace spanning all four checkouts makes local builds resolve
against your working tree while CI resolves against tags. That is the whole reason
`docs/SETUP.md` requires all seven repos cloned side by side under `~/gitops-lab/` on
both machines — relative workspace paths only resolve if the layout matches.

```
~/gitops-lab/
├── bo-service-kit/      <- you are here
├── bo-storefront/
├── bo-catalog/
└── bo-pricing/
```

**`go.work` and `go.work.sum` are local, not shared.** They describe one machine's
checkout layout. Keep them gitignored.

## Metrics discipline — this is where it is enforced

**Never label a Prometheus metric with a commit SHA, image digest, or Rollout hash.**
These are unbounded: a metric carrying every digest ever deployed will quietly consume
the whole store. SHAs belong in GitHub Deployments and Discord messages, where
cardinality is free.

Since every metric in the lab is defined in this repo, this rule is enforceable in
exactly one place. Keep it that way.

## What must never live here

- Business logic belonging to one service
- Anything importing a service repo — dependencies point one way only
## Non-negotiable (inherited from `bo-platform/CLAUDE.md`)

- **No floating tags. Ever.** Not `latest`, `lts`, `stable`, or partial semver (`:1`,
  `:1.2`). Images pinned by **manifest-list digest**, charts by exact semver.
- **Pin the index digest, never a per-arch digest.** A platform-specific digest pulls
  fine on one machine and fails `no match for platform` on the other. This is the most
  likely portability bug in the lab.
- **Cross-platform, always.** Everything must work on `darwin/arm64` (MacBook, the
  runtime target) and `linux/amd64` (Windows/WSL2, build and test only).
- **LF line endings**, enforced by `.gitattributes`. A CRLF `.sh` inside a Linux image
  fails as `bad interpreter: /bin/bash^M`.
- **When something fails, check architecture first** — the usual cause of
  `ImagePullBackOff` and `exec format error` here.
- If reality contradicts the plan, **stop and say so.** Do not improvise around it;
  record the outcome in `bo-platform/docs/DECISIONS.md`.

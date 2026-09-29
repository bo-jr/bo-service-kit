# bo-service-kit

The shared Go module every service imports. `telemetry/`, `chaos/`, `httpx/`.

Canonical spec: [`bo-platform/BUILD-PLAN.md`](https://github.com/bo-jr/bo-platform/blob/main/BUILD-PLAN.md)
§4 (layout), Phase 2 (the services). Where it and
[`bo-platform/DECISIONS.md`](https://github.com/bo-jr/bo-platform/blob/main/DECISIONS.md)
disagree, `DECISIONS.md` wins.

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

**The OTLP exporter must never block a request, crash a service, or fail `/readyz`** when
no collector is listening. Alloy does not exist until Phase 4, and even then it can be
down. Trace IDs must still be generated and propagated with no exporter at all.

## Get the chaos interface right early, then leave it alone

This is the code you will iterate on most, and across a polyrepo split every change
otherwise becomes: tag the kit, `go get -u` in three repos, three PRs.

**Use the workspace.** Each service repo is its own module and imports this one at a
**tag**. A `go.work` at `~/git/go.work` spans all four checkouts, so local builds resolve
against your working tree while Docker and CI — whose build context holds one repo —
resolve against tags. That only works because all seven repos are cloned side by side:

```
~/git/
├── go.work              <- local only, never committed
├── bo-service-kit/      <- you are here
├── bo-storefront/
├── bo-catalog/
└── bo-pricing/
```

**`go.work` and `go.work.sum` are local, not shared.** They describe one machine's
checkout layout. They are gitignored here and in each service repo.

**Tags are immutable.** This module is public, so the first fetch of a tag records its
hash in `sum.golang.org`. Never move a tag; a fix is a new patch version.

## Metrics discipline — this is where it is enforced

**Never label a Prometheus metric with a commit SHA, image digest, or Rollout hash.**
These are unbounded: a metric carrying every digest ever deployed will quietly consume
the whole store. SHAs belong in GitHub Deployments and Discord messages, where
cardinality is free. `APP_VERSION` is a human version — `v1`, `v2`.

Since every metric in the lab is defined in this repo, this rule is enforceable in
exactly one place. Keep it that way.

## What must never live here

- Business logic belonging to one service
- Anything importing a service repo — dependencies point one way only

## Non-negotiable (inherited from `bo-platform/CLAUDE.md`)

- **No floating tags. Ever.** Not `latest`, `lts`, `stable`, or partial semver (`:1`,
  `:1.2`). Images pinned by **manifest-list digest**, charts by exact semver.
- **Pin the index digest, never a per-arch digest.** GitHub Actions runners are
  `linux/amd64`; every cluster in the lab is `arm64`. A platform-specific digest pulls
  fine where you tested it and fails `no match for platform` on the other side of that
  boundary — which anything CI builds crosses on every run. This is the most likely
  portability bug in the lab.
- **LF line endings**, enforced by `.gitattributes`. A CRLF `.sh` inside a Linux image
  fails as `bad interpreter: /bin/bash^M`.
- **When something fails, check architecture first** — the usual cause of
  `ImagePullBackOff` and `exec format error` here.
- If reality contradicts the plan, **stop and say so.** Do not improvise around it;
  record the outcome in `bo-platform/DECISIONS.md`.

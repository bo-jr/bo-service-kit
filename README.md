# bo-service-kit

Shared Go module for all three services.

Part of a three-cluster GitOps lab demonstrating progressive delivery gated on
**error-budget burn rate**. Spec: [`bo-platform/BUILD-PLAN.md`](https://github.com/bo-jr/bo-platform/blob/main/BUILD-PLAN.md) ·
Working rules: [`CLAUDE.md`](./CLAUDE.md)

`telemetry/` (OTel + structured logs), `chaos/` (failure and latency knobs), `httpx/` (server scaffolding). Defines every Prometheus metric in the lab, once.

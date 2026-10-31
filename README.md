# CLIProxyAPI Reset-Aware Scheduler Plugin

This ABI v1 scheduler plugin routes Claude requests to the credential whose
**weekly quota resets soonest**, so quota that is about to expire is spent
before quota that would carry over to the next week.

It ranks credentials from the `Anthropic-Ratelimit-Unified-*` headers that
CLIProxyAPI already records from every Claude response. It makes no upstream
calls of its own.

## Host requirement

The host must pass each candidate's quota snapshot in
`SchedulerAuthCandidate.Metadata["quota_signals"]`. Stock CLIProxyAPI does not
do this yet. On a host without it, every scheduled request fails with
`candidate ... has no quota_signals metadata`, so a wrong image is caught right away.

## Selection

Within the tier the host offers (priorities are respected):

1. Only `claude` candidates with an observed weekly window are ranked.
   Candidates are skipped when their weekly window is `rejected`, their 5h window is
   `rejected` and has not reset yet, or less than `min_remaining` of the weekly quota is left.
2. If a snapshot's weekly reset has already passed, the window is treated as
   fresh (0% used, next reset one week later).
3. The earliest weekly reset wins. Resets within `tie_window_minutes` of the earliest
   count as tied, and the one with the most quota remaining wins.
4. If nothing can be ranked (no Claude candidates, never-used credentials,
   API keys), the plugin declines and the host's configured policy picks.

Picks the plugin makes skip the host selector. That means the session affinity
and weighted round-robin settings do not apply to them. Because the ranking is
deterministic, traffic stays on one credential until it is drained or rate
limited, which keeps prompt caches warm much like fill-first.

A credential that has never served a request has no snapshot and is used only
when no ranked credential is available.

## Build

```bash
make test
```

Produces `build/reset-aware-scheduler.so`.

## Install from the management interface

Add this registry URL to `plugins.store-sources`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/esauvisky/CLIProxyAPI-Reset-Aware-Scheduler-Plugin/main/registry.json"
```

Only one scheduler plugin is active at a time.

## Example `config.yaml`

```yaml
plugins:
  enabled: true
  configs:
    reset-aware-scheduler:
      enabled: true
      min_remaining: 0.02
      tie_window_minutes: 60
```

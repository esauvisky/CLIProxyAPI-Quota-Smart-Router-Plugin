# CLIProxyAPI Reset-Aware Scheduler Plugin

This ABI v1 scheduler plugin routes Claude requests to the credential whose
**weekly quota resets soonest**, so quota that is about to expire is spent
before quota that would carry over to the next week.

Plain Claude models are ranked from the `Anthropic-Ratelimit-Unified-*`
headers CLIProxyAPI already records from every Claude response. Optional
`spread/<model>` virtual models rotate a chosen credential pool across
providers, and a dashboard shows every value the plugin uses.

## Host requirement

The host must pass each candidate's quota snapshot in
`SchedulerAuthCandidate.Metadata["quota_signals"]` and forward
`ModelRouteResponse.TargetAuthID`. Stock CLIProxyAPI does not
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

## Spread pool (`spread/<model>`)

Models listed in `spread.models` are published again as `<prefix><model>`
(default `spread/`). A request for one of them rotates across the credentials in
`spread.credentials`, from any provider that serves the model (for example
Claude OAuth and Antigravity for `claude-opus-4-8`).

Each pool credential is ranked by

```
rate = weight * remaining / hours_until_reset
```

of its longest quota window that applies to the model. A credential that resets
tomorrow with 30% left (1.25%/h) beats one that resets in 6 days with 60% left
(0.42%/h), so accounts with different reset days are compared fairly. `weight`
(default 1) scales for plan size, e.g. 20 for Max 20x and 1 for Pro. A credential
is skipped while disabled, cooling down, not serving the model, without quota
data, or when any applicable window has less than `min_remaining` left.

Sessions are sticky: a session stays on its credential while prompts keep
coming. After `idle_minutes` (default 5) without a prompt the prompt cache is
cold anyway, so the next prompt is ranked again and may move. If the chosen
credential fails, the host retry picks the next best pool credential of the same
provider.

Quota comes from the same provider usage endpoints the Management Center quota
cards use (Claude, Codex, Antigravity, Kimi, xAI), called through the CPA
management `api-call` endpoint, plus `quota/fetch` for plugin providers such as
OpenCode Go. Snapshots refresh every `management.refresh_minutes` in the
background.

The router hands its credential choice to the scheduler through
`ModelRouteResponse.TargetAuthID`, which also requires the patched host.

## Dashboard

**Reset-Aware Scheduler** in the Management UI menu shows every credential with
its quota windows, pool membership, weight and passive header signals, the
active session bindings with their idle countdown, and the recent routing
decisions with the full ranking behind each one.

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
      management:
        url: "http://127.0.0.1:8317"
        key: "<management key>"
        refresh_minutes: 5
      spread:
        prefix: "spread/"
        idle_minutes: 5
        models: ["claude-opus-4-8"]
        credentials:
          - id: "claude-82849846-claude1@pokemod.dev.json"
            provider: "claude"
            weight: 20
          - id: "antigravity-esauvisky@gmail.com.json"
            provider: "antigravity"
            weight: 5
```

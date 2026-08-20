# Project status (resume-after-a-break cheat sheet)

> Personal quick-reference, not project documentation for other readers —
> see [docs/sources/architecture-and-features.md](docs/sources/architecture-and-features.md)
> for the real docs. This file just exists so picking this project back up
> after a while doesn't mean re-deriving all of this from git log.
>
> Last updated: 2026-08-20, branch `version_1.0` @ `bd469c2` (+ this
> session's uncommitted doc/test additions on top).

## TL;DR

`pola`/`polad` is a stateful active PCEP PCE (Go). Everything built this
session is **merged into `version_1.0`** and passing its own test suite.
The only thing genuinely left hanging is one test result I never got back
(see "Open item" below) — everything else is done and verified.

## What to do first when you come back

1. `git pull origin version_1.0` (and check `github` remote too — both
   should be in sync).
2. Skim [docs/sources/architecture-and-features.md](docs/sources/architecture-and-features.md)
   top to bottom — it's the full architecture/feature reference and stays
   current (there's a skill, `update-pola-docs`, that's supposed to keep it
   that way — check its "Last synced" marker against `git log` to see if
   anything drifted while you were away).
3. Rebuild and run `tools/manual-tests/test-full.sh` against the lab once
   it's reachable again — see [its README](tools/manual-tests/README.md).
   That's the fastest way to confirm nothing regressed.
4. Resolve the open item below before trusting waypoints/loose-source-routing
   completely.

## What was built this session (all merged into `version_1.0`)

- **CSPF node exclusion** — per-policy `exclude` (by router ID or SID) on
  `type: dynamic` SR policies, hard-excluding a router from path computation
  entirely. Rejects excluding a policy's own src/dst/waypoint as a clean
  request-time error, not a silent no-path result.
- **Global node-exclusion set** — `pola node-exclude add/remove/list`,
  server-wide, applied on top of every dynamic policy's own exclude
  automatically. Merged in fresh at every CSPF call (creation +
  reoptimization), never persisted into a policy's own stored intent, so
  toggling it takes effect immediately and reverts cleanly. Silently
  skipped for a policy whose own src/dst happens to be the globally
  excluded node, rather than breaking that policy.
- Both are config-gated by `global.nodeExclusionPersistence` (same
  enable/disable/default-path convention as the pre-existing
  `intentPersistence`).
- **`docs/sources/architecture-and-features.md`** — the full architecture
  and feature reference (PCEP, BGP-LS/TED, SR policy management, intent
  persistence/reoptimization, gRPC API, CLI, config, tools, testing,
  deployment), plus a from-scratch "Core Concepts" primer at the top for
  anyone new to PCE/PCEP/SR-TE.
- **`.claude/skills/update-pola-docs/`** — a Claude Code skill that keeps
  that doc in sync with the code going forward (re-verifies against source,
  doesn't just trust memory).
- **`tools/manual-tests/`** — two bash scripts (`test-node-exclusion.sh`,
  `test-full.sh`) for driving a real lab through the `pola` CLI, covering
  everything from session/TED reads through explicit/dynamic paths, both
  exclusion mechanisms, live reoptimization, intent persistence across a
  real `polad` restart, session delete/reconnect, and `tools/sr-mesh`
  idempotency. See their own README for how to run them and what each
  covers.

## Full feature list (with where to read more)

| Feature | Detail |
|---|---|
| PCEP session mgmt, vendor interop (Nokia/FRR/Cisco/Juniper) | architecture doc §3 |
| BGP-LS/TED via GoBGP | architecture doc §4 |
| Explicit SR policies (router-ID form, endpoint-address form, `--no-sid-validate`) | architecture doc §5 |
| Dynamic SR policies (CSPF, metrics igp/te/delay, loose source routing/waypoints) | architecture doc §5 |
| Per-policy + global node exclusion | architecture doc §5 |
| Intent persistence (durable, survives restart) + native reoptimization | architecture doc §6 |
| gRPC API (`CreateSRPolicy`, ..., `AddExcludedNode`, `RemoveExcludedNode`, `GetExcludedNodes`) | architecture doc §7 |
| `pola` CLI (`session`, `sr-policy`, `ted`, `node-exclude`) | architecture doc §8, `cmd/pola/README.md` |
| Config reference | architecture doc §9, `docs/schemas/server/polad_config.json` |
| `tools/sr-mesh` full-mesh provisioner | architecture doc §10 |
| `tools/topology-viewer` web GUI | **not merged** — branch `add-topology-viewer` |

## Open item — needs resolving, not yet closed out

**Loose source routing (waypoints) test failure, root cause not yet
confirmed.** During a `test-full.sh` run, Test 6 (waypoints) failed: a
dynamic policy created with an explicit `waypoints: [{routerID: X}]`
constraint computed a path that did **not** contain waypoint `X`'s own SID
at all — which should be structurally impossible if
`CSPFWithLooseSourceRouting` (`pkg/cspf/cspf.go`) is working correctly,
since it always appends the waypoint's own segment.

What's been ruled out or made unlikely:
- Not the general "read too fast" race that caused most other failures in
  earlier runs (that class of bug was fixed — see `wait_for_policy` in the
  test script).
- Not a stale leftover policy from a previous run — a pre-emptive
  `delete_policy` cleanup was added at the start of every run specifically
  to rule this out.
- A *different* SRC/DST/WAYPOINT combination (auto-discovered on a
  different run) passed correctly earlier in this same session, so it's
  not a wholesale break of the feature — either a real edge case for
  specific node combinations, or still some other test-script artifact not
  yet identified.

**Last action taken**: added a diagnostic line to `test-full.sh` (prints
the full policy JSON + the resolved waypoint SID on failure) and handed it
back to re-run — **the result of that re-run was never reported back**.
That's the very next thing to chase: re-run `test-full.sh`, and if Test 6
still fails, use the new diagnostic output plus `pola ted -j` (to inspect
the real topology around whatever router got auto-discovered as the
waypoint) to determine whether this is a genuine bug in
`CSPFWithLooseSourceRouting`/`buildSectionSegments` or a script artifact.
Don't assume either answer without that evidence — do the same
verify-before-theorizing pass this session used throughout.

## Quick command reference

```bash
# Build
go build -o ~/go/bin/polad ./cmd/polad
go build -o ~/go/bin/pola ./cmd/pola

# Run polad
~/go/bin/polad -f ~/pola-run/polad.yaml

# Automated tests (no live lab needed)
go test ./...           # or: make test
make test-race
make test-scenario       # needs Containerlab

# Manual lab tests (needs a live polad + PCCs)
tools/manual-tests/test-node-exclusion.sh   # quick, exclusion-only
tools/manual-tests/test-full.sh             # comprehensive

# Docs
docs/sources/architecture-and-features.md   # the real reference
```

# Project status (resume-after-a-break cheat sheet)

> Personal quick-reference, not project documentation for other readers —
> see [docs/sources/architecture-and-features.md](docs/sources/architecture-and-features.md)
> for the real docs. This file just exists so picking this project back up
> after a while doesn't mean re-deriving all of this from git log.
>
> Last updated: 2026-08-20, branch `version_1.0` @ `8a18c82`.

## TL;DR

`pola`/`polad` is a stateful active PCEP PCE (Go). Everything built this
session is **merged into `version_1.0`**, and a full `tools/manual-tests/test-full.sh`
run against the real lab came back **all tests passed** — nothing left
hanging from this session. The "Future work" section below is the actual
backlog for next time.

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
   That's the fastest way to confirm nothing regressed while you were gone.
4. Look at "Future work" below and pick where to pick up.

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

## Future work (backlog for next session)

Not started — flagged during this session as worth doing, in no particular
order:

1. **Onboard all lab nodes as PCCs.** Currently only three routers
   (`SBLABO12`, `SRLABA14`, `SBLABO10`) run PCEP sessions to `polad` — the
   rest of the topology (see the router table in
   [`docs/sources/architecture-and-features.md`](docs/sources/architecture-and-features.md)
   / the topology-viewer's `router_names.json`) isn't PCC-enabled yet.
   Widening this changes what `tools/sr-mesh` and the manual test scripts'
   auto-discovery actually cover.
2. **Add link latency/delay metrics — static first, then dynamic via
   TWAMP.** This directly relates to the `metric: delay` gap the test
   suite already found: this lab's BGP-LS data currently carries no TE or
   delay metric at all (IGP only), so `metric: delay`/`te` dynamic policies
   can't compute. Start with static delay values configured on the
   IGP/BGP-LS side, then look at TWAMP (RFC 5357-style two-way active
   measurement) for real dynamic delay measurement feeding BGP-LS.
3. **Investigate link-flap behavior.** What actually happens to reoptimization,
   in-flight PCUpds, and the TED when a link flaps repeatedly in a short
   window — is the 5s BGP-LS debounce (§4 of the architecture doc) enough,
   does `reoptimizeMu`'s non-overlap guard hold up, does anything thrash or
   miss a settle point? Not tested at all yet.
4. **Test TI-LFA interaction.** Topology-Independent Loop-Free Alternate is
   an IGP-level local fast-reroute mechanism — worth understanding/testing
   how it interacts with `polad`'s own dynamic reoptimization (does IGP
   TI-LFA already handle transient failures before BGP-LS/reoptimization
   even reacts? do the two ever fight each other?).
5. **Keep building out the topology-viewer GUI** (branch
   `add-topology-viewer`, §10/§14 of the architecture doc) — not merged
   into `version_1.0` yet, still has room for more work before that
   decision.

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

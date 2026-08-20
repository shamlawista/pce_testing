# Manual lab test scripts

These are **not** part of the automated CI/scenario suite (see
[`test/`](../../test) and `make test-scenario` for that — real Containerlab
topologies, run by CI). These two scripts are for exercising a **real, live
`polad` + PCC lab** by hand — they drive it entirely through the `pola` CLI
and gRPC API, the same way an operator would, and were built and iterated on
against the actual lab described in
[docs/sources/architecture-and-features.md](../../docs/sources/architecture-and-features.md).

They both:
- Require `pola`/`jq` in `PATH`, and a running, reachable `polad`.
- Auto-discover generic SR-capable nodes/sessions from the live TED where
  possible, but have a config block at the top with lab-specific
  router IDs/SIDs/paths that **must be reviewed and adjusted** if your
  topology has changed since these were written (see the comments in each
  file — they document exactly which topology facts they assume and why).
- Are non-destructive by default where the action is risky: they pause and
  ask before closing a live PCEP session, and never touch anything without
  cleaning up the test policies they created (best-effort, at the end,
  regardless of pass/fail).
- Print a `PASS`/`WARN`/`FAIL` summary at the end; exit `0` only if nothing
  failed (a `WARN` is informational, e.g. "this metric isn't present in your
  topology" — not a defect).

## `test-node-exclusion.sh` — quick, focused

Covers just the CSPF node-exclusion feature (per-policy `exclude` by
router ID/SID, the global node-exclusion set, creation-time and
reoptimization-time behavior). Faster, lower-risk (no session delete, no
`polad` restart, no mesh provisioning) — use this for a quick confidence
check on that one feature area.

```bash
./test-node-exclusion.sh
```

## `test-full.sh` — comprehensive regression pass

Covers everything: session/TED reads, explicit paths (router-ID form,
`--no-sid-validate`, endpoint-address form), dynamic paths (all metrics,
loose source routing/waypoints), both node-exclusion mechanisms, live
reoptimization, intent persistence across an automated `polad` restart,
session delete + reconnect, and `tools/sr-mesh` idempotency. This is the one
to run before/after a long gap away from the project, to confirm nothing
regressed.

```bash
./test-full.sh
```

It pauses three times for input:
1. Twice for a real topology change (shut/no-shut an interface) to trigger
   a BGP-LS/TED update and exercise live reoptimization.
2. Once for a y/N confirmation before closing a live PCEP session (Test 11).

It also **restarts `polad` itself** (kills the process found via
`pgrep -x polad` and relaunches it from `POLAD_BIN -f POLAD_CONFIG`) to prove
intent persistence survives a real restart — make sure those two paths at
the top of the script actually point at your setup before running it.

## If something fails

Read the `FAIL`/`WARN` line itself first — most things it checks are
described in detail in
[architecture-and-features.md](../../docs/sources/architecture-and-features.md)
(§5 for node exclusion and dynamic/explicit paths, §6 for intent persistence
and reoptimization, §10 for `tools/sr-mesh`). If a failure looks like it
might be a real regression rather than a stale config value, that's the
starting point for a deeper investigation, not a normal outcome to script
around.

---
name: update-pola-docs
description: Refresh docs/sources/architecture-and-features.md so it accurately reflects the current pola/polad codebase. Use proactively right after making a change to this repo's PCEP/gRPC/CSPF/config/CLI/tooling behavior, and whenever the user asks to update, refresh, or sync the pola documentation.
---

# Update Pola docs

`docs/sources/architecture-and-features.md` is a living architecture and
feature reference for this repo (PCEP support, BGP-LS/TED integration, SR
policy management, intent persistence/reoptimization, gRPC API, CLI,
config, tools, deployment). It is meant to stay accurate without the
reader having to cross-check it against the code — that's this skill's job.

## When to run this

- Proactively, immediately after implementing or changing anything that
  the doc describes: a new/changed gRPC RPC or proto message, a new/changed
  `pola` CLI command or flag, a new/changed `polad.yaml` config field, new
  PCEP/CSPF/vendor-interop behavior, a change to intent persistence or the
  reoptimization sweep, or a change under `tools/`.
- Whenever the user explicitly asks to update, refresh, or sync the docs.
- Run it once the change is finished and tested, not mid-implementation.

## How to do it

1. **Read the whole doc first.** Don't skim — you need its existing
   structure and wording to make a minimal, consistent edit, not a rewrite.

2. **Find the sync marker** near the top:
   `> Last synced: branch <name> @ <short-hash> (<date>)`.

3. **Diff since that marker** to scope what actually changed:
   ```bash
   git log --oneline <hash>..HEAD
   git diff <hash>..HEAD --stat
   ```
   If the marker's commit isn't reachable from HEAD (e.g. you're on a
   different branch than last time), fall back to reasoning from the
   current conversation's changes plus a targeted look at whatever files
   you just touched.

4. **For every changed area, re-read the current source** — proto file,
   config struct, CLI command file, the actual Go code — rather than
   relying on memory of an earlier conversation. Verify field names,
   defaults, and behavior directly; this doc is a source of truth for
   readers, so a stale or guessed detail is worse than a missing one.

5. **Update only the affected section(s).** Match the existing doc's
   structure, table formats, and factual/concise tone — don't reorganize
   unrelated sections or change prose style elsewhere. Add a new `##`
   section only when a change introduces a genuinely new area that doesn't
   fit any existing one.

6. **Branch-scoped features**: if a change lives only on a branch not yet
   merged into `version_1.0`, describe it under §13 ("Feature branches in
   development") and add a one-line pointer from the relevant main section
   (mirror the existing node-exclusion / topology-viewer entries) — don't
   present an unmerged feature as generally available. Once a branch gets
   merged into `version_1.0`, move its content out of §13 into the proper
   section and delete the §13 entry.

7. **Update the sync marker** to the branch/commit/date you just verified
   against.

8. **Don't commit/push automatically.** Leave the file changes staged for
   the user to review, and follow this repo's established pattern of
   asking before `git commit`/`git push` — unless the user's request for
   this run explicitly said to just commit it.

9. **Report back concisely**: which section(s) changed and why, in 1-3
   sentences — not a full diff dump.

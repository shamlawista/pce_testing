# Pola PCE — Architecture & Feature Guide

> **Living document.** Kept in sync with the codebase by the `update-pola-docs`
> skill (`.claude/skills/update-pola-docs/SKILL.md`) — run it after making
> changes that affect behavior described here.
>
> Last synced: branch `version_1.0` @ `cd70311` (2026-08-19)

## 1. What is Pola PCE

Pola is a Path Computation Element (PCE) and PCEP protocol library written in
Go. It runs as `polad`, a daemon that:

- Speaks PCEP to routers (PCCs) as an **active stateful PCE** — it can
  initiate, update, and delete Segment Routing (SR) LSPs, not just passively
  reply to path requests.
- Exposes a **gRPC API** (and the `pola` CLI on top of it) for provisioning
  SR policies, reading session/TED state, and managing operational tooling.
- Optionally maintains a live **Traffic Engineering Database (TED)** fed by
  BGP-LS (via GoBGP), enabling dynamic (CSPF-computed) path provisioning
  instead of only caller-supplied explicit paths.

It supports both **SR-MPLS** and **SRv6** (full-SID and compressed/uSID data
planes).

## 2. High-level architecture

```
                         ┌───────────────────────────┐
  operator / automation  │           pola (CLI)      │
                         └─────────────┬─────────────┘
                                       │ gRPC
                         ┌─────────────▼─────────────┐
                         │           polad            │
                         │  ┌───────────────────────┐ │
                         │  │  gRPC API server      │ │◄── your own controller
                         │  │  (pkg/server, APIServer)│    (see api/pola/v1)
                         │  └──────────┬────────────┘ │
                         │  ┌──────────▼────────────┐ │
   PCEP (RFC 5440) ◄─────┼──┤  PCEP session server   │ │
   routers/PCCs           │  │  (pkg/server, Session) │ │
                         │  └──────────┬────────────┘ │
                         │  ┌──────────▼────────────┐ │
                         │  │  CSPF (pkg/cspf)       │ │
                         │  │  over in-memory TED    │ │
                         │  │  (pkg/table.LsTED)     │ │
                         │  └──────────┬────────────┘ │
                         └─────────────┼───────────────┘
                                       │ gRPC (BGP-LS NLRIs)
                         ┌─────────────▼─────────────┐
                         │           GoBGP             │
                         │  (external BGP-LS speaker)  │
                         └─────────────┬───────────────┘
                                       │ BGP-LS
                                  IGP/BGP-LS-speaking routers
```

### Package map

| Package | Responsibility |
|---|---|
| `cmd/polad` | Daemon entrypoint: loads config, wires everything in `pkg/server.NewPCE`. |
| `cmd/pola` | CLI entrypoint: cobra commands, thin wrapper over the gRPC client. |
| `cmd/pola/grpc` | gRPC client helper functions used by the CLI. |
| `pkg/server` | Core daemon logic: PCEP session handling (`Session`), the daemon-wide `Server`, the gRPC service implementation (`APIServer`), intent persistence. |
| `pkg/packet/pcep` | PCEP wire protocol: message/object/TLV encode-decode, capability negotiation, vendor interop quirks. |
| `pkg/cspf` | Constrained Shortest Path First path computation over the TED. |
| `pkg/table` | Shared data model: TED (`LsTED`/`LsNode`/`LsLink`/`LsPrefix`), `SRPolicy`, `Segment` types, metric/policy-type enums. |
| `internal/gobgp` | Bridges GoBGP's BGP-LS gRPC API into `pkg/table.TEDElem` updates. |
| `internal/config` | `polad.yaml` schema + validation. |
| `api/pola/v1` | Protobuf/gRPC service definition (`pola.proto`) and generated Go bindings. |
| `tools/sr-mesh` | Standalone Python script: full-mesh SR policy provisioner (see §10). |

## 3. PCEP protocol support

Implemented in `pkg/packet/pcep`:

- **Messages** (`message.go`): Open, Keepalive, PCReq, PCRep, Notification,
  Error, Close, PCMonReq/Rep (RFC 5886), Report (RFC 8231 PCRpt), Update
  (RFC 8281 PCUpd), LSP Initiate Request (RFC 8281 PCInitiate), StartTLS
  (RFC 8253) — base protocol per RFC 5440.
- **Objects** (`object.go`): all core RFC 5440 objects (OPEN, RP, NO-PATH,
  ENDPOINTS, BANDWIDTH, METRIC, ERO, RRO, LSPA, IRO, SVEC, NOTIFICATION,
  PCEP-ERROR, LOAD-BALANCING, CLOSE, PATH-KEY, XRO, MONITORING,
  PCC-REQ-ID, OF, CLASS-TYPE, GLOBAL-CONSTRAINTS, PCE-ID, PROC-TIME,
  OVERLOAD, UNREACH-DESTINATION), plus stateful/SR extensions: SERO/SRRO,
  BNC, LSP, SRP, VENDOR-INFORMATION (RFC 7470), BU, and ASSOCIATION
  (RFC 8697, IPv4/IPv6).
- **Capability negotiation** (`capability.go`): `PolaCapability()` builds
  what Pola advertises (always sets the Color capability on; disables
  P2MP/scheduling extensions it doesn't implement).

### Vendor interoperability (PccType)

Not every PCC implements the relevant RFCs identically. `pkg/packet/pcep`
models this as a `PccType` enum, resolved once per session
(`pkg/server/session.go`), with precedence **forced Nokia > forced FRR >
auto-detected**:

| PccType | How it's selected | What's different |
|---|---|---|
| `RFCCompliant` | Default | Standard RFC 8697 ASSOCIATION object, no special-casing. |
| `CiscoLegacy` / `JuniperLegacy` | Auto-detected from the advertised `AssocTypeList` capability (vendor-specific `AssociationType` codes: Cisco `0x14`, Juniper `0xffe1`) | Vendor-specific association encoding for pre-RFC-9256 SR policy signaling. |
| `FRRoutingLegacy` | Cannot be auto-detected — FRR advertises normal RFC-compliant capabilities. Set explicitly via `PCEOptions.FRRPeers` / `pcep.frrPeers` in config. | PCInitiate carries a Cisco-format VENDOR-INFORMATION blob alongside the RFC-compliant ASSOCIATION object, for FRRouting builds predating full RFC 9256 support. |
| `NokiaLegacy` | Cannot be auto-detected. Set explicitly via `PCEOptions.NokiaPeers` / `pcep.nokiaPeers` in config. | PCInitiate **omits the ASSOCIATION object entirely**. A live Nokia 7750 (SR OS 26.7.R1) was observed closing the PCEP session (error reason 3, "malformed PCEP message") when ASSOCIATION carried the RFC 9862 SRPOLICY-CPATH-ID/PREFERENCE TLVs — even though RFC 5440 §7.1 mandates silently ignoring unrecognized TLVs. That Nokia release doesn't implement RFC 9862. Also: Nokia SR OS doesn't advertise the Color capability back. |

Configure `frrPeers`/`nokiaPeers` under `global.pcep` in `polad.yaml` (see §9).

## 4. BGP-LS / TED integration

Dynamic (CSPF-computed) SR policies require a live TED. Pola doesn't speak
BGP itself — it delegates that to **GoBGP**, which must be configured with
the `ls` (BGP-LS) AFI/SAFI, and connects to GoBGP's own gRPC API
(`internal/gobgp/interface.go`):

1. **Initial sync**: on startup, `MonitorBGPLsEvents` fetches the full
   current set of BGP-LS NLRIs from GoBGP.
2. **Live updates**: it then opens a `WatchEvent` stream and, on each BGP-LS
   change notification, **debounces for 5 seconds** (coalescing bursts of
   updates from a single IGP/BGP-LS convergence event) before re-fetching
   the full NLRI set and rebuilding the TED from scratch.
3. Every rebuilt TED is pushed to `pkg/server.Server` (`setTED` +
   `propagateTED` to every live PCEP session) **and** triggers the native
   reoptimization sweep (§6) — a topology change can immediately reroute
   existing dynamic policies away from a now-suboptimal or now-invalid path.

The TED itself (`pkg/table`) is a plain in-memory model, not a generic graph
library:

- `LsTED` — `map[string]*LsNode` keyed by router ID.
- `LsNode` — ASN, RouterID, ISIS area ID, hostname (if BGP-LS carried one),
  SRGB (`SrgbBegin`/`SrgbEnd`), and the node's links/prefixes/SRv6 SIDs.
- `LsLink` — local/remote `*LsNode`, local/remote IP, IGP/TE/delay metrics,
  Adjacency-SID, optional SRv6 End.X SID.
- `LsPrefix` — the node's advertised prefixes; a prefix with `HasSidIndex`
  set is the node's loopback / Prefix-SID (Node-SID).

Config (`polad.yaml`):

```yaml
global:
  ted:
    enable: true
    source: "gobgp"
    asn: 65000
  gobgp:
    grpcClient:
      address: "127.0.0.1"
      port: 50051
```

GoBGP itself needs the BGP-LS AFI/SAFI enabled per neighbor — see
[Getting Started](getting-started.md#case-ted-enable) for a full GoBGP
config example. **IPv6 underlay (IPv6 SR-MPLS / SRv6) is not currently
supported for TED-driven dynamic paths.**

## 5. SR Policy management

An SR policy (`pkg/table.SRPolicy`) has a `Type`: `explicit` or `dynamic`
(RFC 9256 §2.4.2 candidate-path source distinction).

### Explicit paths

The caller supplies the full segment list; Pola does no path computation.
Two request forms:

- **Router-ID form** (`srcRouterID`/`dstRouterID`): endpoints resolved from
  the TED. Each SID is validated against the TED unless
  `--no-sid-validate` is passed (or `ted.enable: false`, which forces it).
- **Endpoint-address form** (`srcAddr`/`dstAddr`): bypasses the TED/router-ID
  resolution entirely — used when TED is disabled or endpoints aren't
  TED-resolvable.

Each `Segment` is one of:
- `SegmentSRMPLS` — a label, optionally carrying NAI (local/remote address,
  or unnumbered/link-local interface IDs per RFC 8664 §4.3.1).
- `SegmentSRv6` — a full 128-bit SID or compressed uSID, optional NAI, and
  SID Structure (Locator/Node/Function/Argument split).

### Dynamic paths

Pola runs CSPF (`pkg/cspf`) over the live TED:

- `metric`: `igp` | `te` | `delay` — the IGP-style link metric CSPF
  minimizes (`hopcount` also exists as a `MetricType` value).
- Path computation walks only SR-capable nodes (nodes with a resolvable
  Node-SID from their SRGB); a non-SR-capable transit node is pruned from
  consideration rather than aborting the whole computation, unless it's the
  path's own source (which is a hard error — you can't originate an SR path
  from a node with no Node-SID).
- **Loose source routing** (`waypoints`): a list of `{routerID, sid?}`
  entries the computed path must transit, in order — CSPF decomposes the
  request into per-section shortest paths between consecutive waypoints
  (`CSPFWithLooseSourceRouting`). If a waypoint's `sid` is omitted, its
  Node-SID is looked up from the TED.

### Node exclusion

Two independent mechanisms hard-exclude routers from CSPF consideration
entirely — treated as absent from the graph, not merely deprioritized:

- **Per-policy `exclude`** (`type: dynamic` only): a list of routers to keep
  out of consideration for that one policy specifically, e.g. to route
  around a node ahead of a planned migration. Each entry names a router by
  `routerID` or by `sid` (resolved against the TED to a router ID at
  request time) — exactly one of the two per entry. Rejected up front
  (clean error, not a "no path found") if it names the policy's own
  `srcRouterID`/`dstRouterID` or an explicit `waypoints[].routerID`. This
  set is persisted like `type`/`metric` and reapplied on every
  reoptimization triggered by a later topology change.
- **Global node-exclusion set** (`pola node-exclude add/remove/list`,
  backed by `global.nodeExclusionPersistence`): a server-wide "avoid this
  node everywhere" list, applied on top of every dynamic policy's own
  `exclude` automatically — so avoiding a node ahead of a migration doesn't
  require editing every individual policy. Merged in fresh at the moment
  each CSPF call happens (creation and every reoptimization), never
  persisted into any individual policy's own stored intent — so adding or
  removing a node from the global set takes effect immediately across
  every dynamic policy, and reverts just as cleanly. If a globally-excluded
  node happens to be a specific policy's own source, destination, or an
  explicit waypoint, the global exclusion is silently skipped for that one
  policy (its own `exclude` still applies normally) rather than breaking
  it — see `mergeGlobalExclude` in `pkg/server/global_exclude_store.go`.

Both forms can leave *no* valid path (e.g. combined with a topology that
only has one or two alternate routers) — CSPF reports this as a clean
"no SR-MPLS path found" error at creation time, or (during reoptimization)
logs it and leaves the currently-installed LSP untouched rather than
tearing it down.

### Color / preference

Every SR policy carries a `color` (candidate-path selection per RFC 9256)
and a `preference` (defaults to 100).

## 6. Intent persistence & reoptimization

PCEP itself is not very good at telling you *why* a path was chosen — a
PCRpt reports the installed segment list, not the metric/exclusions that
produced it. Pola tracks that "intent" at two durability tiers
(`pkg/server`):

1. **Ephemeral, SRP-ID-keyed** (`srPolicyIntent`, in `Session`): records
   type/metric/exclude the moment a PCInitiate/PCUpdate is sent, keyed by
   SRP-ID, with a TTL — enough to correlate the *next* PCRpt reply to what
   was actually requested. Doesn't survive a PCEP resync (state-sync PCRpts
   report SRP-ID 0) or a restart.
2. **Durable, (peer, policy-name)-keyed** (`intentStore`): a JSON file,
   written atomically (temp file + rename), that survives a `polad`
   restart. Controlled by `global.intentPersistence` in config (enabled by
   default, `/var/lib/pola/intents.json`).

Both feed **native reoptimization**: every time the TED changes (§4),
`Server.reoptimizeDynamicPolicies` re-runs CSPF for every currently-known
`type: dynamic` policy using its remembered type/metric(/exclude), across
every synced PCEP session, and pushes a PCUpdate if the computed path
actually changed. This is guarded against overlapping sweeps
(`Server.reoptimizeMu.TryLock`) and is what makes dynamic policies durable
against migrations/failures without any external controller re-provisioning
them.

## 7. gRPC API

Defined in `api/pola/v1/pola.proto`, service `PCEService`:

| RPC | Purpose |
|---|---|
| `CreateSRPolicy` | Provision (or update, if the name already exists on that session) an SR policy. |
| `DeleteSRPolicy` | Tear down an SR policy. |
| `GetSessionList` | List PCEP sessions and their negotiated capabilities. |
| `GetSRPolicyList` | List known SR policies, grouped by session. |
| `GetTED` | Dump the current TED. |
| `DeleteSession` | Forcibly close a PCEP session. |
| `AddExcludedNode` | Add a router (by router ID or SID) to the global node-exclusion set (§5). |
| `RemoveExcludedNode` | Remove a router from the global node-exclusion set. |
| `GetExcludedNodes` | List the current global node-exclusion set. |

Regenerate Go bindings after editing the `.proto` with `make proto` (buf
under the hood); `make check-proto` verifies generated code is current.
See `examples/grpc/go` for a from-scratch Go client, or `cmd/pola` for the
reference client implementation.

## 8. CLI (`pola`) reference

Full detail and JSON examples: [cmd/pola/README.md](../../cmd/pola/README.md).

| Command | Purpose |
|---|---|
| `pola session [-j]` | List PCEP sessions. |
| `pola session delete <address> [-j]` | Close a session. |
| `pola sr-policy list [-j] [--session <addr>]` | List SR policies. |
| `pola sr-policy add -f <file> [--no-sid-validate]` | Create/update an SR policy from YAML. |
| `pola sr-policy delete -f <file>` | Delete an SR policy. |
| `pola ted [-j]` | Dump the TED. |
| `pola node-exclude add --routerID <id>\|--sid <sid>` | Add a router to the global node-exclusion set (§5). |
| `pola node-exclude remove --routerID <id>\|--sid <sid>` | Remove a router from the global node-exclusion set. |
| `pola node-exclude list [-j]` | List the global node-exclusion set. |

Global flags: `--host` (default `127.0.0.1`), `-p`/`--port` (default
`50051`), `-j`/`--json`.

## 9. Configuration reference (`polad.yaml`)

Schema: [docs/schemas/server/polad_config.json](../schemas/server/polad_config.json).
Full walkthrough: [Getting Started](getting-started.md).

| Field | Purpose |
|---|---|
| `global.pcep.address` / `.port` | PCEP listener (literal IP only, no hostnames). |
| `global.pcep.frrPeers` / `.nokiaPeers` | Force `PccType` for peers that can't be auto-detected (§3). |
| `global.grpcServer.address` / `.port` | gRPC API listener. |
| `global.log.path` / `.name` / `.debug` | Log file location and verbosity. |
| `global.ted.enable` | Whether to maintain a TED at all (required for dynamic paths). |
| `global.ted.source` | Currently only `"gobgp"`. |
| `global.ted.asn` | Required when `ted.enable: true`; also used as a request-ASN sanity check on `CreateSRPolicy`. |
| `global.gobgp.grpcClient.address` / `.port` | GoBGP's own gRPC API, for BGP-LS NLRIs. |
| `global.usidMode` | Encode SRv6 SIDs as compressed uSIDs. |
| `global.intentPersistence.enable` / `.path` | Durable SR policy intent store (§6). Enabled by default at `/var/lib/pola/intents.json`; a missing/unwritable directory only disables the feature for that run (logged, non-fatal), never crashes startup. |
| `global.nodeExclusionPersistence.enable` / `.path` | Durable global node-exclusion set (§5). Same enabled-by-default / non-fatal-degradation behavior as `intentPersistence`, at `/var/lib/pola/node-exclusions.json`. |

## 10. Tools

### `tools/sr-mesh` — mesh provisioner

A standalone Python script (`sr_mesh_provision.py` / `mesh_lib.py`), not
part of `polad` itself. Discovers every synced PCEP session and every
SR-capable TED node (by shelling out to `pola session -j` / `pola ted -j`,
via `subprocess`, not gRPC directly), computes the desired full mesh of
`type: dynamic` SR policies (one per session → destination pair not already
present), and provisions any missing ones via `pola sr-policy add`. CSPF
itself is left entirely to `polad`. Idempotent — matches by policy name and
skips ones that already exist, so it's safe to re-run after a topology
change or a `polad` restart. No separate "reoptimize" daemon is needed
alongside it: `polad`'s native TED-triggered reoptimization (§6) already
keeps existing dynamic policies current.

Key flags: `--host`, `--port`, `--asn`, `--color`, `--metric`
(igp/te/delay), `--name-prefix`, `--pola-bin`, `-v`.

### `tools/topology-viewer` — web GUI

A Flask + Cytoscape.js web app for visualizing the live TED (nodes, links,
Node-SIDs) with click-to-inspect SR policies and click-to-highlight LSP
paths. **Lives on branch `add-topology-viewer`, not merged into
`version_1.0`** (§13) — not present in this branch's working tree.

## 11. Deployment

- Binary: `go install github.com/nttcom/pola/cmd/polad@latest` /
  `.../cmd/pola@latest`, or build from source (`go install ./...`).
- Docker: [build/package/README.md](../../build/package/README.md) — covers
  the shared `intentPersistence`/`nodeExclusionPersistence` volume mount.
- Containerlab examples (`examples/containerlab/`): shared prerequisites in
  its top-level README, then per-scenario:
  - `sr-mpls-explicit-path/` — explicit-path SR-MPLS (IOS-XR/Junos/FRRouting).
  - `sr-mpls-explicit-path-l3vpn/` — explicit SR-MPLS TE + VPNv4 (FRRouting).
  - `srv6-explicit-path-l3vpn/` — explicit SRv6 TE + VPNv4/VPNv6 (Juniper vJunos-router).
  - `srv6-usid-dynamic-path/` — dynamic (CSPF) SRv6 uSID (Cisco XRd).
  - `srv6-usid-dynamic-path-loose-source-routing-sfc/` — dynamic SRv6 uSID with loose-hop waypoints for SFC (Cisco XRd).

## 12. Development

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for the full guide. Key `make`
targets: `setup`, `build`, `fmt`/`fix`, `lint`, `test`, `test-race`, `proto`,
`check-proto`, `image`/`image-debug`, `ci`, `test-scenario`(`-parallel`)
(containerlab + pytest scenario tests), `clean`.

## 13. Feature branches in development (not yet merged into `version_1.0`)

These exist in the repo's remotes but aren't part of `version_1.0` yet, so
they're intentionally described only briefly here rather than as if
already merged:

- **`add-topology-viewer`** — the web GUI described in §10.

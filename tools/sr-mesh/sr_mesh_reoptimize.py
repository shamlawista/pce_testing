#!/usr/bin/env python3
"""Long-running watch-and-reoptimize daemon for mesh-* dynamic SR policies.

This fork has no re-optimization logic at all: once a `type: dynamic` SR
policy is provisioned, its segment list never gets re-evaluated even though
polad's TED keeps updating live from BGP-LS. This daemon closes that gap for
policies created by sr_mesh_provision.py.

There is no dry-run / compute-only RPC in this fork's gRPC API (see
api/pola/v1/pola.proto), and `sr-policy add`'s update path
(pkg/server/grpc_server.go: sendSRPolicyRequest -> SendPCUpdate) sends a real
PCUpd to the router unconditionally -- it never compares against the
currently installed segment list first. So calling `add` is never free:
every call is a real re-signal (same PlspID, incremented LSPID), whether or
not the path actually changed. To avoid needless PCEP churn, this daemon
predicts what `add` would compute *before* calling it, using cspf_replica.py
-- a tracked, read-only mirror of pkg/cspf/cspf.go's Dijkstra run against the
same `ted -j` data polad itself would use. `add` is only actually invoked for
a policy whose predicted segment list differs from the one currently
installed. The TED fingerprint (see --interval / --polad-log below) is only
a cheap pre-filter to skip the whole per-policy prediction pass when nothing
in the TED changed at all; it does not by itself decide whether any
individual policy gets re-signaled.

Trigger modes (used together):
  - Polling: every --interval seconds (default 45s), always active.
  - Log-tail: if --polad-log points at a readable file, "Update TED" debug
    lines (pkg/server/server.go) trigger an immediate check. Requires
    log.debug: true in polad.yaml. If the file is missing or unreadable,
    this is skipped and polling alone drives the loop.

On each triggered check, only if the TED fingerprint changed since the last
check does it walk installed mesh-* policies (via `pola sr-policy list -j`)
and re-add each one whose destination is still a valid mesh endpoint. A
policy whose destination dropped out of the SR-capable set (or its session
went down) is left in place and logged as stale, not deleted or retried.

Usage:
    python3 sr_mesh_reoptimize.py --port 50052
    python3 sr_mesh_reoptimize.py --port 50052 --once   # single pass, for testing
"""
from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import signal
import sys
import threading
import time
from pathlib import Path

from cspf_replica import CSPFError, spf
from mesh_lib import (
    PolaCLI,
    PolaCLIError,
    build_desired_mesh,
    resolve_pola_bin,
    resolve_session_nodes,
    sr_capable_nodes,
    valid_sessions,
)

log = logging.getLogger("sr_mesh_reoptimize")


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", default="127.0.0.1", help="polad gRPC host (default: 127.0.0.1)")
    p.add_argument("--port", default="50052", help="polad gRPC port (default: 50052)")
    p.add_argument("--asn", type=int, default=65018, help="ASN used to rebuild the desired mesh (default: 65018)")
    p.add_argument("--color", type=int, default=100, help="color used to rebuild the desired mesh (default: 100)")
    p.add_argument("--metric", default="igp", choices=["igp", "te", "delay"], help="CSPF metric (default: igp)")
    p.add_argument("--name-prefix", default="mesh", help="only manage policies named '<prefix>-...' (default: mesh)")
    p.add_argument(
        "--pola-bin",
        default="pola",
        help="path to the pola binary (default: 'pola' on PATH, falling back to ~/go/bin/pola)",
    )
    p.add_argument("--interval", type=float, default=45.0, help="fallback poll interval in seconds (default: 45)")
    p.add_argument(
        "--settle-delay",
        type=float,
        default=6.0,
        help="seconds to wait after a trigger before checking, to clear GoBGP's BGP-LS debounce (default: 6)",
    )
    p.add_argument(
        "--polad-log",
        default="/var/log/pola/polad.log",
        help="polad log file to tail for 'Update TED' triggers; skipped if missing/unreadable "
        "(default: /var/log/pola/polad.log)",
    )
    p.add_argument("--no-log-tail", action="store_true", help="disable log tailing, poll on --interval only")
    p.add_argument("--once", action="store_true", help="run a single check pass and exit (for testing)")
    p.add_argument("-v", "--verbose", action="store_true", help="enable debug logging")
    return p.parse_args()


def ted_fingerprint(ted_nodes: list[dict]) -> str:
    normalized = sorted(ted_nodes, key=lambda n: n.get("routerID", ""))
    blob = json.dumps(normalized, sort_keys=True, default=str)
    return hashlib.sha256(blob.encode()).hexdigest()


def start_log_tail(path: str, trigger_event: threading.Event, stop_event: threading.Event) -> threading.Thread | None:
    log_path = Path(path)
    if not log_path.exists():
        log.info("polad log %s not found, relying on --interval polling only", path)
        return None
    try:
        f = open(log_path, "r", encoding="utf-8", errors="ignore")
    except OSError as e:
        log.info("cannot read polad log %s (%s), relying on --interval polling only", path, e)
        return None

    def _tail() -> None:
        log.info("tailing %s for 'Update TED' events", path)
        f.seek(0, os.SEEK_END)
        while not stop_event.is_set():
            line = f.readline()
            if not line:
                time.sleep(1)
                continue
            if "Update TED" in line:
                trigger_event.set()
        f.close()

    thread = threading.Thread(target=_tail, name="polad-log-tail", daemon=True)
    thread.start()
    return thread


def run_cycle(cli: PolaCLI, args: argparse.Namespace, last_fingerprint: str | None) -> str | None:
    try:
        sessions = cli.sessions()
        ted_nodes = cli.ted()
    except PolaCLIError as e:
        log.error("skipping check: %s", e)
        return last_fingerprint

    fingerprint = ted_fingerprint(ted_nodes)
    if fingerprint == last_fingerprint:
        log.debug("TED unchanged since last check, nothing to do")
        return fingerprint
    log.info("TED changed since last check, re-checking %s* policies", args.name_prefix)

    up_sessions = valid_sessions(sessions)
    resolved, _unresolved = resolve_session_nodes(up_sessions, ted_nodes, log)
    sr_nodes = sr_capable_nodes(ted_nodes)
    desired = build_desired_mesh(resolved, sr_nodes, args.asn, args.color, args.metric, args.name_prefix)
    desired_by_key = {(p.session_addr, p.name): p for p in desired}
    node_by_id = {n["routerID"]: n for n in ted_nodes if n.get("routerID")}

    try:
        installed = cli.sr_policy_list()
    except PolaCLIError as e:
        log.error("could not list SR policies, skipping this check: %s", e)
        return fingerprint

    changed = unchanged = stale = no_path = errors = 0
    prefix = f"{args.name_prefix}-"
    for peer in installed:
        peer_addr = peer.get("peerAddr")
        for pol in peer.get("srPolicies", []):
            name = pol.get("policyName", "")
            if not name.startswith(prefix):
                continue  # not one of ours, leave it alone

            desired_policy = desired_by_key.get((peer_addr, name))
            if desired_policy is None:
                log.warning(
                    "%-40s on %s no longer resolves to a valid mesh endpoint "
                    "(session down or destination not SR-capable) -- leaving in place, flagged stale",
                    name, peer_addr,
                )
                stale += 1
                continue

            old_segments = pol.get("segmentList", [])
            old_sids = [seg.get("sid") for seg in old_segments]

            try:
                predicted_sids = spf(
                    desired_policy.src_router_id, desired_policy.dst_router_id, desired_policy.metric, node_by_id
                )
            except CSPFError as e:
                # e.g. a transit/destination node just lost its Node SID -- an
                # expected topology state (mirrors cspf.go's own pruning/error
                # semantics), not a script error. Leave the installed LSP alone
                # rather than re-add into a path that no longer computes.
                log.info("%-40s on %s: %s -- leaving installed path as-is", name, peer_addr, e)
                no_path += 1
                continue

            if predicted_sids == old_sids:
                unchanged += 1
                continue

            log.info("%-40s on %s: predicted path changed %s -> %s, reoptimizing", name, peer_addr, old_sids, predicted_sids)
            result = cli.sr_policy_add(desired_policy.yaml())
            if not result.ok:
                log.error("%-40s reoptimize FAILED on %s: %s", name, peer_addr, result.reason)
                errors += 1
                continue

            new_segments = _fetch_segment_list(cli, peer_addr, name)
            new_sids = [seg.get("sid") for seg in new_segments] if new_segments is not None else None
            if new_sids is not None and new_sids != predicted_sids:
                log.warning(
                    "%-40s on %s: polad's installed path %s differs from this script's prediction %s "
                    "-- cspf_replica.py may have drifted from pkg/cspf/cspf.go, please check",
                    name, peer_addr, new_sids, predicted_sids,
                )
            log.info("REOPTIMIZED %s on %s: %s -> %s", name, peer_addr, old_sids, new_sids if new_sids is not None else predicted_sids)
            changed += 1

    log.info(
        "check complete: %d reoptimized, %d unchanged, %d stale, %d no-path, %d error(s)",
        changed, unchanged, stale, no_path, errors,
    )
    return fingerprint


def _fetch_segment_list(cli: PolaCLI, peer_addr: str, name: str) -> list | None:
    try:
        peers = cli.sr_policy_list(session=peer_addr)
    except PolaCLIError as e:
        log.error("could not re-fetch %s on %s after recompute: %s", name, peer_addr, e)
        return None
    for peer in peers:
        for pol in peer.get("srPolicies", []):
            if pol.get("policyName") == name:
                return pol.get("segmentList", [])
    return None


def main() -> None:
    args = parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)-7s %(message)s",
    )

    cli = PolaCLI(resolve_pola_bin(args.pola_bin), args.host, args.port)

    stop_event = threading.Event()
    trigger_event = threading.Event()
    tail_thread = None
    if not args.no_log_tail:
        tail_thread = start_log_tail(args.polad_log, trigger_event, stop_event)

    def handle_signal(signum, _frame) -> None:
        log.info("received signal %d, shutting down", signum)
        stop_event.set()
        trigger_event.set()

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)

    last_fingerprint = None
    while True:
        last_fingerprint = run_cycle(cli, args, last_fingerprint)
        if args.once or stop_event.is_set():
            break
        woke_on_trigger = trigger_event.wait(timeout=args.interval)
        trigger_event.clear()
        if stop_event.is_set():
            break
        if woke_on_trigger:
            time.sleep(args.settle_delay)

    if tail_thread is not None:
        stop_event.set()
        tail_thread.join(timeout=2)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""One-shot full-mesh SR-TE policy provisioner for a Pola PCE lab.

Discovers every up+synced PCEP session and every SR-capable node in the
polad TED (via the `pola` CLI), then provisions one `type: dynamic` SR
policy per ordered (session, destination) pair that doesn't already exist.
CSPF path computation is left entirely to polad.

Safe to re-run: policies are matched by name per session and left alone if
already present, so this can be re-run after new nodes join the TED or
after a polad restart wipes learned policies.

Does not modify polad/pola; it only shells out to the existing `pola` CLI.

Usage:
    python3 sr_mesh_provision.py --port 50052
"""
from __future__ import annotations

import argparse
import logging
import sys

from mesh_lib import (
    PolaCLI,
    PolaCLIError,
    build_desired_mesh,
    resolve_pola_bin,
    resolve_session_nodes,
    sr_capable_nodes,
    valid_sessions,
)

log = logging.getLogger("sr_mesh_provision")


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", default="127.0.0.1", help="polad gRPC host (default: 127.0.0.1)")
    p.add_argument("--port", default="50052", help="polad gRPC port (default: 50052)")
    p.add_argument("--asn", type=int, default=65018, help="ASN of provisioned policies (default: 65018)")
    p.add_argument("--color", type=int, default=100, help="color of provisioned policies (default: 100)")
    p.add_argument("--metric", default="igp", choices=["igp", "te", "delay"], help="CSPF metric (default: igp)")
    p.add_argument("--name-prefix", default="mesh", help="policy name prefix (default: mesh)")
    p.add_argument(
        "--pola-bin",
        default="pola",
        help="path to the pola binary (default: 'pola' on PATH, falling back to ~/go/bin/pola)",
    )
    p.add_argument("-v", "--verbose", action="store_true", help="enable debug logging")
    return p.parse_args()


def print_summary(
    total_sessions: int,
    sr_capable_count: int,
    skipped_non_sr: int,
    desired: list,
    succeeded: list,
    already_existed: list,
    failed: list,
) -> None:
    print()
    print("=" * 64)
    print("SR-TE Full-Mesh Provisioning Summary")
    print("=" * 64)
    print(f"{'PCEP sessions found':<32}{total_sessions}")
    print(f"{'SR-capable TED nodes found':<32}{sr_capable_count} (skipped {skipped_non_sr} non-SR node(s))")
    print(f"{'Policies attempted':<32}{len(desired)}")
    print(f"{'  succeeded':<32}{len(succeeded)}")
    print(f"{'  already existed':<32}{len(already_existed)}")
    print(f"{'  failed':<32}{len(failed)}")
    if failed:
        print("-" * 64)
        print("Failures (for follow-up):")
        for policy, reason, category in failed:
            tag = "expected-no-path" if category == "no_path" else "ERROR"
            print(f"  [{tag}] {policy.name}  ({policy.session_addr} -> {policy.dst_router_id})")
            print(f"      {reason}")
    print("=" * 64)


def main() -> None:
    args = parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)-7s %(message)s",
    )

    cli = PolaCLI(resolve_pola_bin(args.pola_bin), args.host, args.port)

    try:
        sessions = cli.sessions()
        ted_nodes = cli.ted()
    except PolaCLIError as e:
        log.error("%s", e)
        sys.exit(1)

    up_sessions = valid_sessions(sessions)
    log.info("discovered %d PCEP session(s), %d up+synced", len(sessions), len(up_sessions))

    resolved, _unresolved = resolve_session_nodes(up_sessions, ted_nodes, log)

    sr_nodes = sr_capable_nodes(ted_nodes)
    skipped_non_sr = len(ted_nodes) - len(sr_nodes)
    log.info(
        "TED has %d node(s), %d SR-capable (skipped %d non-SR node(s))",
        len(ted_nodes), len(sr_nodes), skipped_non_sr,
    )

    desired = build_desired_mesh(resolved, sr_nodes, args.asn, args.color, args.metric, args.name_prefix)
    log.info("full mesh requires %d ordered policy pair(s)", len(desired))

    try:
        existing = cli.sr_policy_list()
    except PolaCLIError as e:
        log.error("%s", e)
        sys.exit(1)
    existing_names = {peer["peerAddr"]: {pol["policyName"] for pol in peer.get("srPolicies", [])} for peer in existing}

    succeeded, already_existed, failed = [], [], []
    for policy in desired:
        if policy.name in existing_names.get(policy.session_addr, set()):
            log.info("%-40s already exists on %s, skipping", policy.name, policy.session_addr)
            already_existed.append(policy)
            continue

        result = cli.sr_policy_add(policy.yaml())
        if result.ok:
            log.info("%-40s provisioned (%s -> %s)", policy.name, policy.session_addr, policy.dst_router_id)
            succeeded.append(policy)
        else:
            reason = result.reason
            category = "no_path" if "no path found" in reason.lower() else "error"
            log.error("%-40s FAILED (%s -> %s): %s", policy.name, policy.session_addr, policy.dst_router_id, reason)
            failed.append((policy, reason, category))

    print_summary(len(sessions), len(sr_nodes), skipped_non_sr, desired, succeeded, already_existed, failed)

    if any(category == "error" for _, _, category in failed):
        sys.exit(2)


if __name__ == "__main__":
    main()

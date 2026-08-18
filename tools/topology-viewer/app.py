#!/usr/bin/env python3
"""Live topology/LSP viewer for a Pola PCE.

Polls `pola ted -j` / `pola session -j` / `pola sr-policy list -j` on an
interval and serves a small web UI: the BGP-LS-derived topology with node
SIDs (continuously refreshed), click a node to see the policies (LSPs)
terminating or transiting it, click a policy to highlight the path it
actually takes on the graph.

Does not modify polad/pola; it only shells out to the existing `pola` CLI,
same as tools/sr-mesh.

Usage:
    pip install flask
    python3 app.py --port 50052                 # against a live polad
    python3 app.py --mock                       # synthetic demo data, no pola/polad needed
"""
from __future__ import annotations

import argparse
import json
import logging
import threading
import time
from pathlib import Path

from flask import Flask, jsonify, send_from_directory

from pola_client import PolaCLIError, PolaClient
from topology import build_graph, enrich_policies

log = logging.getLogger("topology_viewer")

STATIC_DIR = Path(__file__).parent / "static"
DEFAULT_NAME_OVERRIDES_PATH = Path(__file__).parent / "router_names.json"


def load_name_overrides(path: Path) -> dict:
    """routerID -> display name, for labs where BGP-LS isn't advertising a
    hostname at all (see router_names.json / topology.build_label_map).
    Missing file is normal (not every lab has one); a malformed one is
    logged and treated as empty rather than crashing the whole app.
    """
    if not path.exists():
        return {}
    try:
        overrides = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as e:
        log.warning("could not read name overrides from %s: %s", path, e)
        return {}
    if not isinstance(overrides, dict):
        log.warning("name overrides file %s must be a JSON object of routerID -> name, ignoring", path)
        return {}
    return overrides


class Snapshot:
    """Latest polled state, guarded by a lock (read from Flask request
    threads, written from the background poll thread)."""

    def __init__(self):
        self._lock = threading.Lock()
        self._graph = {"nodes": [], "edges": []}
        self._policies = []
        self._updated_at = None
        self._error = "not yet polled"

    def set(self, graph, policies):
        with self._lock:
            self._graph = graph
            self._policies = policies
            self._updated_at = time.time()
            self._error = None

    def set_error(self, message):
        with self._lock:
            self._error = message

    def get(self):
        with self._lock:
            return dict(self._graph), list(self._policies), self._updated_at, self._error


def poll_loop(client: PolaClient, snapshot: Snapshot, interval: float, stop_event: threading.Event, name_overrides: dict):
    while not stop_event.is_set():
        try:
            ted_nodes = client.ted()
            sessions = client.sessions()
            policies_by_session = client.sr_policy_list()

            session_router_ids = set()
            addr_to_router = {}
            for node in ted_nodes:
                for prefix in node.get("prefixes", []):
                    p = prefix.get("prefix", "")
                    if p.endswith("/32") or "/128" in p:
                        addr_to_router[p.split("/")[0]] = node["routerID"]
            for s in sessions:
                router_id = addr_to_router.get(s.get("Addr"))
                if router_id:
                    session_router_ids.add(router_id)

            graph = build_graph(ted_nodes, session_router_ids, name_overrides)
            policies = enrich_policies(policies_by_session, ted_nodes)
            snapshot.set(graph, policies)
            log.info("poll ok: %d node(s), %d edge(s), %d polic(y/ies)",
                      len(graph["nodes"]), len(graph["edges"]), len(policies))
        except PolaCLIError as e:
            log.error("poll failed: %s", e)
            snapshot.set_error(str(e))

        stop_event.wait(interval)


def build_mock_snapshot(node_count: int | None = None, name_overrides: dict | None = None) -> Snapshot:
    if node_count:
        from mock_data import build_large_mock_policies, build_large_mock_sessions, build_large_mock_ted

        ted_nodes = build_large_mock_ted(node_count)["ted"]
        sessions = build_large_mock_sessions(node_count)
        policies_by_session = build_large_mock_policies(ted_nodes, sessions)
    else:
        from mock_data import build_mock_policies, build_mock_sessions, build_mock_ted

        ted_nodes = build_mock_ted()["ted"]
        sessions = build_mock_sessions()
        policies_by_session = build_mock_policies()

    snapshot = Snapshot()
    session_addrs = {s["Addr"] for s in sessions}
    session_router_ids = {
        n["routerID"] for n in ted_nodes
        if any(p.get("prefix", "").split("/")[0] in session_addrs for p in n.get("prefixes", []))
    }
    graph = build_graph(ted_nodes, session_router_ids, name_overrides)
    policies = enrich_policies(policies_by_session, ted_nodes)
    snapshot.set(graph, policies)
    return snapshot


def create_app(snapshot: Snapshot) -> Flask:
    app = Flask(__name__, static_folder=None)

    @app.get("/")
    def index():
        return send_from_directory(STATIC_DIR, "index.html")

    @app.get("/<path:filename>")
    def static_files(filename):
        return send_from_directory(STATIC_DIR, filename)

    @app.get("/api/state")
    def api_state():
        graph, policies, updated_at, error = snapshot.get()
        return jsonify({
            "graph": graph,
            "policies": policies,
            "updatedAt": updated_at,
            "error": error,
        })

    return app


def parse_args():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", default="127.0.0.1", help="polad gRPC host (default: 127.0.0.1)")
    p.add_argument("--port", default="50052", help="polad gRPC port (default: 50052)")
    p.add_argument("--pola-bin", default="pola", help="path to the pola binary (default: 'pola' on PATH)")
    p.add_argument("--interval", type=float, default=5.0, help="poll interval in seconds (default: 5)")
    p.add_argument("--listen-host", default="0.0.0.0", help="web UI bind address (default: 0.0.0.0)")
    p.add_argument("--listen-port", type=int, default=8080, help="web UI port (default: 8080)")
    p.add_argument("--mock", action="store_true", help="serve synthetic demo data instead of polling a live polad")
    p.add_argument("--mock-nodes", type=int, default=None,
                    help="with --mock, generate a synthetic ring-plus-chords topology with this many nodes "
                         "instead of the small built-in 6-node example (useful for checking layout/label "
                         "behavior at a realistic node count)")
    p.add_argument("--name-overrides", default=str(DEFAULT_NAME_OVERRIDES_PATH),
                    help="path to a JSON object mapping routerID -> display name, for labs where BGP-LS "
                         f"isn't advertising a hostname at all (default: {DEFAULT_NAME_OVERRIDES_PATH.name} "
                         "next to this script, if present)")
    p.add_argument("-v", "--verbose", action="store_true")
    return p.parse_args()


def main():
    args = parse_args()
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                         format="%(asctime)s %(levelname)-7s %(message)s")

    name_overrides = load_name_overrides(Path(args.name_overrides))
    if name_overrides:
        log.info("loaded %d name override(s) from %s", len(name_overrides), args.name_overrides)

    if args.mock:
        log.info("running with synthetic mock data (--mock); no pola/polad required")
        snapshot = build_mock_snapshot(args.mock_nodes, name_overrides)
        app = create_app(snapshot)
        app.run(host=args.listen_host, port=args.listen_port)
        return

    snapshot = Snapshot()
    client = PolaClient(args.pola_bin, args.host, args.port)
    stop_event = threading.Event()
    poll_thread = threading.Thread(
        target=poll_loop, args=(client, snapshot, args.interval, stop_event, name_overrides), daemon=True,
    )
    poll_thread.start()

    app = create_app(snapshot)
    try:
        app.run(host=args.listen_host, port=args.listen_port)
    finally:
        stop_event.set()


if __name__ == "__main__":
    main()

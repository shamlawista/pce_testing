"""Pure, backend-framework-free logic: turn `pola ted -j` / `pola sr-policy
list -j` JSON into the node/edge graph and per-LSP highlight paths the
frontend renders. No I/O here - keeps this independently testable and reusable
from both the real Flask app and mock_data.py.
"""
from __future__ import annotations


def sanitize_name(value: str) -> str:
    return value.replace(".", "-").replace(":", "-").strip("-")


def build_label_map(ted_nodes: list[dict]) -> dict[str, str]:
    """Map each node's routerID to a unique, human-readable display label.

    Prefers the advertised hostname, falling back to routerID for any
    hostname shared by more than one node in this TED snapshot (ISIS/BGP-LS
    hostnames aren't protocol-guaranteed unique - see tools/sr-mesh/mesh_lib.py
    for the incident this guards against: two different nodes with the same
    hostname would otherwise be indistinguishable in the UI).
    """
    hostname_counts: dict[str, int] = {}
    for node in ted_nodes:
        hostname = (node.get("hostname") or "").strip()
        if hostname:
            hostname_counts[hostname] = hostname_counts.get(hostname, 0) + 1

    labels = {}
    for node in ted_nodes:
        router_id = node["routerID"]
        hostname = (node.get("hostname") or "").strip()
        if hostname and hostname_counts[hostname] == 1:
            labels[router_id] = hostname
        else:
            labels[router_id] = router_id
    return labels


def node_segment_label(node: dict):
    """Mirror pkg/table.LsNode.NodeSegment(): the SR-MPLS label (srgbBegin +
    sidIndex) of the first prefix carrying a Prefix-SID, falling back to the
    first SRv6 SID. None if the node has no Node SID.
    """
    for prefix in node.get("prefixes", []):
        if "sidIndex" in prefix and prefix["sidIndex"] is not None:
            return node.get("srgbBegin", 0) + prefix["sidIndex"]
    for srv6_sid in node.get("srv6SIDs", []):
        sids = srv6_sid.get("sids") or []
        if sids:
            return sids[0]
    return None


def is_sr_capable(node: dict) -> bool:
    if not node.get("srgbBegin") or not node.get("srgbEnd"):
        return bool(node.get("srv6SIDs"))
    return any("sidIndex" in prefix and prefix["sidIndex"] is not None for prefix in node.get("prefixes", []))


def build_sid_index(ted_nodes: list[dict]) -> dict:
    """Map each node's own Node-SID/SRv6-SID value to its routerID, for
    resolving a segment list's bare node SIDs back to the node they name."""
    index = {}
    for node in ted_nodes:
        label = node_segment_label(node)
        if label is not None:
            index[label] = node["routerID"]
    return index


def build_adjacency_index(ted_nodes: list[dict]) -> dict:
    """Map (localAddr, remoteAddr) -> the routerID reached by traversing that
    adjacency, for resolving NAI-carrying (explicit) adjacency SIDs that a
    bare node-SID lookup can't handle. Keyed by the exact address pair each
    node reports for its own end of the link.
    """
    index = {}
    for node in ted_nodes:
        for link in node.get("links", []):
            local_ip = link.get("localIP")
            remote_ip = link.get("remoteIP")
            remote_node = link.get("remoteNode")
            if local_ip and remote_ip and remote_node:
                index[(local_ip, remote_ip)] = remote_node
    return index


def dedupe_edges(ted_nodes: list[dict]) -> list[dict]:
    """BGP-LS reports each link once per direction (each node lists its own
    local->remote adjacency); collapse that into one undirected edge per
    connected pair for display, keeping the first-seen metric/adjSid.
    """
    edges: dict[tuple, dict] = {}
    for node in ted_nodes:
        for link in node.get("links", []):
            remote = link.get("remoteNode")
            if not remote:
                continue
            key = tuple(sorted((node["routerID"], remote)))
            if key in edges:
                continue
            igp_metric = None
            for metric in link.get("metrics", []):
                if metric.get("type") == "METRIC_TYPE_IGP":
                    igp_metric = metric.get("value")
                    break
            edges[key] = {
                "id": f"{key[0]}__{key[1]}",
                "source": key[0],
                "target": key[1],
                "metric": igp_metric,
            }
    return list(edges.values())


def build_graph(ted_nodes: list[dict], session_router_ids: set[str]) -> dict:
    """Build the {nodes, edges} JSON shape the frontend graph library consumes."""
    labels = build_label_map(ted_nodes)
    nodes = []
    for node in ted_nodes:
        router_id = node["routerID"]
        nodes.append({
            "id": router_id,
            "label": labels[router_id],
            "sid": node_segment_label(node),
            "srCapable": is_sr_capable(node),
            "isSession": router_id in session_router_ids,
            "asn": node.get("asn"),
        })
    return {"nodes": nodes, "edges": dedupe_edges(ted_nodes)}


def resolve_policy_path(policy: dict, sid_index: dict, adjacency_index: dict) -> dict:
    """Best-effort resolve a policy's bare segment list back into an ordered
    list of routerIDs it traverses, for highlighting on the topology graph.

    Two resolution strategies per segment, tried in order:
      1. Adjacency match: if the segment carries localAddr/remoteAddr (an
         explicit NAI-based adjacency SID), look up which node that specific
         link lands on - the most precise resolution available.
      2. Node-SID match: otherwise treat the bare `sid` value as a node's own
         Node-SID/SRv6-SID label (this is what pola's own CSPF always emits
         for `type: dynamic` policies - see pkg/cspf/cspf.go's
         buildSegmentListFromPath, which only ever appends NodeSegment()).

    Segments that resolve to neither are reported separately in
    `unresolved` rather than silently dropped or guessed at, so the UI can
    flag a path as partially-drawn instead of presenting a wrong one as
    if it were complete.

    The result is a list of *waypoints*, not necessarily a walkable path: a
    single node-SID segment means "IGP shortest path to this node," which
    can legitimately cross several physical links with no separate segment
    for each one. Expanding waypoints into an actual hop-by-hop route is
    expand_to_physical_path()'s job, not this function's.
    """
    waypoints: list[str] = []
    unresolved: list = []

    src = policy.get("srcRouterId")
    if src:
        waypoints.append(src)

    def append_hop(router_id):
        if not waypoints or waypoints[-1] != router_id:
            waypoints.append(router_id)

    for seg in policy.get("segmentList", []):
        local_addr = seg.get("localAddr")
        remote_addr = seg.get("remoteAddr")
        if local_addr and remote_addr and (local_addr, remote_addr) in adjacency_index:
            append_hop(adjacency_index[(local_addr, remote_addr)])
            continue

        sid = seg.get("sid")
        router_id = sid_index.get(sid)
        if router_id is not None:
            append_hop(router_id)
        else:
            unresolved.append(sid)

    dst = policy.get("dstRouterId")
    if dst:
        append_hop(dst)

    return {"waypoints": waypoints, "unresolved": unresolved}


METRIC_TYPE_JSON = {
    "igp": "METRIC_TYPE_IGP",
    "te": "METRIC_TYPE_TE",
    "delay": "METRIC_TYPE_DELAY",
    "hopcount": "METRIC_TYPE_HOPCOUNT",
}


def _link_metric(link: dict, metric_key: str):
    want = METRIC_TYPE_JSON.get(metric_key, "METRIC_TYPE_IGP")
    for metric in link.get("metrics", []):
        if metric.get("type") == want:
            return metric.get("value")
    return None


def shortest_path_between(src: str, dst: str, metric_key: str, node_by_id: dict, sr_capable_ids: set) -> list[str] | None:
    """Dijkstra from src to dst, restricted to SR-capable transit nodes -
    mirroring pkg/cspf/cspf.go's own pruning of neighbors without a Node SID
    (a non-SR-capable router can't do SR-MPLS label forwarding, so the real
    LSP can never actually transit one either). dst itself is always allowed
    even if it isn't SR-capable, matching cspf.go's "destination reachability
    is a separate concern from transit capability" split.

    Used only to reconstruct, for display, the physical route a single
    node-SID waypoint pair represents - not to compute or validate anything
    the PCE itself relies on. Returns None if no such path exists.
    """
    if src == dst:
        return [src]
    if src not in node_by_id or dst not in node_by_id:
        return None

    cost = {src: 0}
    prev = {src: None}
    visited = set()

    while True:
        candidate = None
        for node_id, c in cost.items():
            if node_id in visited:
                continue
            if candidate is None or c < cost[candidate]:
                candidate = node_id
        if candidate is None:
            return None
        if candidate == dst:
            break
        visited.add(candidate)

        for link in node_by_id[candidate].get("links", []):
            remote = link.get("remoteNode")
            if not remote or remote in visited or remote not in node_by_id:
                continue
            if remote != dst and remote not in sr_capable_ids:
                continue
            metric = _link_metric(link, metric_key)
            if metric is None:
                continue
            new_cost = cost[candidate] + metric
            if remote not in cost or new_cost < cost[remote]:
                cost[remote] = new_cost
                prev[remote] = candidate

    path = []
    cur = dst
    while cur is not None:
        path.append(cur)
        cur = prev[cur]
    path.reverse()
    return path


def expand_to_physical_path(waypoints: list[str], metric_key: str, node_by_id: dict, sr_capable_ids: set) -> dict:
    """Stitch consecutive waypoints into an actual walkable node path by
    Dijkstra-ing between each pair, so the frontend can highlight the real
    links traversed instead of a phantom direct edge that may not exist.

    Segments where no path could be found between two consecutive waypoints
    (a genuine topology gap, not expected in normal operation) fall back to
    the bare 2-node hop and are flagged in `approximate` rather than silently
    dropped.
    """
    if not waypoints:
        return {"path": [], "approximate": []}

    full_path = [waypoints[0]]
    approximate = []

    for a, b in zip(waypoints, waypoints[1:]):
        sub_path = shortest_path_between(a, b, metric_key, node_by_id, sr_capable_ids)
        if sub_path is None:
            approximate.append([a, b])
            sub_path = [a, b]
        # sub_path[0] == full_path[-1] (== a); skip it to avoid a duplicate hop
        full_path.extend(sub_path[1:])

    return {"path": full_path, "approximate": approximate}


def edges_for_path(path: list[str]) -> list[str]:
    """Edge ids (matching dedupe_edges' id scheme) for each consecutive hop
    in a resolved node path."""
    edge_ids = []
    for a, b in zip(path, path[1:]):
        key = tuple(sorted((a, b)))
        edge_ids.append(f"{key[0]}__{key[1]}")
    return edge_ids


def enrich_policies(policies_by_session: list[dict], ted_nodes: list[dict]) -> list[dict]:
    """Flatten `sr-policy list -j`'s per-session grouping into one list, each
    policy annotated with its resolved highlight path."""
    sid_index = build_sid_index(ted_nodes)
    adjacency_index = build_adjacency_index(ted_nodes)
    node_by_id = {n["routerID"]: n for n in ted_nodes}
    sr_capable_ids = {n["routerID"] for n in ted_nodes if is_sr_capable(n)}

    enriched = []
    for session in policies_by_session:
        peer_addr = session.get("peerAddr")
        for policy in session.get("srPolicies", []):
            resolved = resolve_policy_path(policy, sid_index, adjacency_index)
            # explicit-type policies (table.PolicyType) may not report a
            # metric at all; IGP is the least surprising default for
            # reconstructing a display-only route.
            metric_key = policy.get("metric") or "igp"
            expanded = expand_to_physical_path(resolved["waypoints"], metric_key, node_by_id, sr_capable_ids)
            enriched.append({
                **policy,
                "peerAddr": peer_addr,
                "path": expanded["path"],
                "pathEdges": edges_for_path(expanded["path"]),
                "unresolvedSegments": resolved["unresolved"],
                "approximateHops": expanded["approximate"],
            })
    return enriched

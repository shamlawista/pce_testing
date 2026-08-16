"""Read-only Python port of pkg/cspf/cspf.go's `spf()` (SR-MPLS, node-segment
only -- no waypoints, no SRv6), operating directly on `pola ted -j` output.

Kept in sync by hand with pkg/cspf/cspf.go and pkg/table/ted.go's
NodeSegment()/Metric(). If polad's CSPF logic changes, this drifts silently
until the parity tests in test_cspf_replica.py (ported from
pkg/cspf/cspf_test.go) or the runtime drift-check in sr_mesh_reoptimize.py
(which compares this module's prediction against what polad actually
installs after every real re-add) catch it.

Known deliberate divergence: Go's `nextNode` breaks cost ties by Go map
iteration order, which is randomized per-run. This port breaks ties by
insertion order instead. Both are valid shortest paths of equal total cost,
so this only affects which specific equal-cost hop sequence is picked in a
tie, not correctness -- and polad itself isn't deterministic in that case
either.

Real `ted -j` link metric types are "METRIC_TYPE_IGP" / "METRIC_TYPE_TE" /
"METRIC_TYPE_DELAY" / "METRIC_TYPE_HOPCOUNT" (see
test/scenario/show-ted/*/expected/*.json) -- the cmd/pola/README.md sample
showing bare "IGP" is stale. This module normalizes either form.
"""
from __future__ import annotations


class CSPFError(Exception):
    """Mirrors the errors pkg/cspf/cspf.go's spf() returns."""


_METRIC_PREFIX = "METRIC_TYPE_"


def _normalize_metric_type(raw: str) -> str:
    upper = raw.upper()
    return upper if upper.startswith(_METRIC_PREFIX) else f"{_METRIC_PREFIX}{upper}"


def _node_segment_sid(node: dict) -> int:
    """Mirrors LsNode.NodeSegment()'s SR-MPLS branch: the Node SID is
    srgbBegin + sidIndex of the first prefix carrying one. Raises CSPFError
    if the node has no Node SID (SRv6-only nodes are out of scope for this
    lab's all-SR-MPLS mesh, same as the rest of this tool)."""
    srgb_begin = node.get("srgbBegin") or 0
    for prefix in node.get("prefixes", []):
        if "sidIndex" in prefix and prefix["sidIndex"] is not None:
            return srgb_begin + prefix["sidIndex"]
    raise CSPFError(f"node {node.get('routerID')} doesn't have a Node SID")


def _link_metric(link: dict, metric_type: str) -> int:
    wanted = _normalize_metric_type(metric_type)
    for m in link.get("metrics", []):
        if _normalize_metric_type(str(m.get("type", ""))) == wanted:
            return m["value"]
    raise CSPFError(f"metric {metric_type} not defined on link to {link.get('remoteNode')}")


class _Node:
    __slots__ = ("id", "calculated", "cost", "prev", "sid")

    def __init__(self, node_id: str, cost: int, sid: int):
        self.id = node_id
        self.calculated = False
        self.cost = cost
        self.prev: str | None = None
        self.sid = sid


def _next_node(calculating: dict[str, _Node]) -> str | None:
    best_id = None
    for node_id, node in calculating.items():
        if node.calculated:
            continue
        if best_id is None or calculating[best_id].cost > node.cost:
            best_id = node_id
    return best_id


def _update_neighbor_costs(calc_id: str, calculating: dict[str, _Node], node_by_id: dict, metric_type: str) -> None:
    node = node_by_id.get(calc_id)
    if node is None:
        return
    for link in node.get("links", []):
        link_cost = _link_metric(link, metric_type)  # hard error if missing, matches cspf.go
        remote_id = link.get("remoteNode")

        if remote_id in calculating:
            remote = calculating[remote_id]
            if calculating[calc_id].cost + link_cost < remote.cost:
                remote.cost = calculating[calc_id].cost + link_cost
                remote.prev = calc_id
            continue

        remote_node = node_by_id.get(remote_id)
        if remote_node is None:
            continue  # not in this TED snapshot at all -- prune like a non-SR neighbor
        try:
            remote_sid = _node_segment_sid(remote_node)
        except CSPFError:
            continue  # prune: neighbor isn't SR-capable, not usable as a transit hop
        new_node = _Node(remote_id, calculating[calc_id].cost + link_cost, remote_sid)
        new_node.prev = calc_id
        calculating[remote_id] = new_node


def _build_segment_list(src_router_id: str, dst_router_id: str, calculating: dict[str, _Node]) -> list[int]:
    segments = []
    node = calculating[dst_router_id]
    while node.id != src_router_id:
        segments.append(node.sid)
        node = calculating[node.prev]
    segments.reverse()
    return segments


def spf(src_router_id: str, dst_router_id: str, metric_type: str, node_by_id: dict[str, dict]) -> list[int]:
    """Predict the ordered list of node-segment SIDs (excluding the source,
    including the destination) that `pola sr-policy add` would compute right
    now for a `type: dynamic` policy with this src/dst/metric.

    node_by_id: {routerID: node-dict}, using the same shape `pola ted -j`
    returns per node (routerID, srgbBegin, srgbEnd, prefixes, links).
    metric_type: "igp" | "te" | "delay" | "hopcount" (case-insensitive).

    Raises CSPFError if no all-SR-MPLS path exists, mirroring cspf.go.
    """
    src_node = node_by_id.get(src_router_id)
    if src_node is None:
        raise CSPFError(f"source router {src_router_id} not found in TED")

    calculating = {src_router_id: _Node(src_router_id, 0, _node_segment_sid(src_node))}

    while True:
        calc_id = _next_node(calculating)
        if calc_id is None:
            raise CSPFError(f"no SR-MPLS path found from {src_router_id} to {dst_router_id}")
        if calc_id == dst_router_id:
            break
        _update_neighbor_costs(calc_id, calculating, node_by_id, metric_type)
        calculating[calc_id].calculated = True

    return _build_segment_list(src_router_id, dst_router_id, calculating)

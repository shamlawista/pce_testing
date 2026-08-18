"""Plain assert-based smoke tests for topology.py, run directly with
`python test_topology.py` - no pytest dependency required for this
standalone tool."""
from mock_data import build_mock_ted, build_mock_policies
from topology import (
    build_adjacency_index,
    build_graph,
    build_label_map,
    build_sid_index,
    dedupe_edges,
    enrich_policies,
    expand_to_physical_path,
    is_sr_capable,
    node_segment_label,
    resolve_policy_path,
    shortest_path_between,
)

ted = build_mock_ted()["ted"]
by_id = {n["routerID"]: n for n in ted}
sr_capable_ids = {n["routerID"] for n in ted if is_sr_capable(n)}

PE1, P1, P2, PE2, PE3, P3_LEGACY = (
    "0000.0000.0001", "0000.0000.0002", "0000.0000.0003",
    "0000.0000.0004", "0000.0000.0005", "0000.0000.0006",
)

# --- node_segment_label / is_sr_capable ---
assert node_segment_label(by_id[PE1]) == 16001, "PE1 should have Node-SID 16001"
assert node_segment_label(by_id[PE2]) == 16004, "PE2 should have Node-SID 16004"
assert node_segment_label(by_id[P3_LEGACY]) is None, "P3-LEGACY has no SRGB/Prefix-SID"
assert is_sr_capable(by_id[PE1]) is True
assert is_sr_capable(by_id[P3_LEGACY]) is False
print("node_segment_label / is_sr_capable: OK")

# --- build_label_map: duplicate hostname falls back to routerID ---
dup_ted = ted + [dict(by_id[PE3], routerID="0000.0000.0099")]  # same hostname "PE3", different routerID
labels = build_label_map(dup_ted)
assert labels[PE3] == PE3, f"expected routerID fallback for duplicate hostname, got {labels[PE3]!r}"
assert labels["0000.0000.0099"] == "0000.0000.0099"
assert labels[PE1] == "PE1", "unique hostname should still be used"
print("build_label_map duplicate-hostname fallback: OK")

# --- dedupe_edges: 6 bidirectional link pairs -> 6 undirected edges, not 12 ---
edges = dedupe_edges(ted)
assert len(edges) == 6, f"expected 6 deduped edges, got {len(edges)}: {edges}"
pe1_p1 = next(e for e in edges if {PE1, P1} == {e["source"], e["target"]})
assert pe1_p1["metric"] == 10
print("dedupe_edges: OK", edges)

# --- build_graph shape ---
graph = build_graph(ted, session_router_ids={PE1, PE2})
assert len(graph["nodes"]) == 6
pe1_node = next(n for n in graph["nodes"] if n["id"] == PE1)
assert pe1_node["isSession"] is True
assert pe1_node["sid"] == 16001
p3_node = next(n for n in graph["nodes"] if n["id"] == P3_LEGACY)
assert p3_node["isSession"] is False
assert p3_node["srCapable"] is False
print("build_graph: OK")

# --- resolve_policy_path: pure node-SID segment list (the CSPF-emitted case) ---
sid_index = build_sid_index(ted)
adjacency_index = build_adjacency_index(ted)
policies = build_mock_policies()

pe1_pe2 = policies[0]["srPolicies"][0]  # sid 16003 (P2), 16004 (PE2); src PE1, dst PE2
resolved = resolve_policy_path(pe1_pe2, sid_index, adjacency_index)
assert resolved["waypoints"] == [PE1, P2, PE2], resolved
assert resolved["unresolved"] == []
print("resolve_policy_path (node-SID waypoints): OK", resolved)

# --- resolve_policy_path: single node-SID segment naming a non-neighbor
# (PE1 and PE3 aren't directly linked - only reachable via P1) ---
pe1_pe3 = policies[0]["srPolicies"][1]  # sid 16005 (PE3) only; src PE1, dst PE3
resolved2 = resolve_policy_path(pe1_pe3, sid_index, adjacency_index)
assert resolved2["waypoints"] == [PE1, PE3], resolved2
print("resolve_policy_path (single-segment waypoints, not yet a walkable path): OK", resolved2)

# --- resolve_policy_path: unresolved SID is reported, not silently dropped ---
bogus_policy = {
    "srcRouterId": PE1,
    "dstRouterId": PE2,
    "segmentList": [{"sid": 99999}],
}
resolved3 = resolve_policy_path(bogus_policy, sid_index, adjacency_index)
assert resolved3["unresolved"] == [99999], resolved3
assert resolved3["waypoints"] == [PE1, PE2], resolved3
print("resolve_policy_path (unresolved segment flagged): OK", resolved3)

# --- resolve_policy_path: adjacency-SID (NAI) resolution ---
adj_policy = {
    "srcRouterId": PE1,
    "dstRouterId": None,
    "segmentList": [{"sid": 24001, "localAddr": "10.0.1.1", "remoteAddr": "10.0.1.2"}],
}
resolved4 = resolve_policy_path(adj_policy, sid_index, adjacency_index)
assert resolved4["waypoints"] == [PE1, P1], resolved4
print("resolve_policy_path (adjacency-SID via NAI): OK", resolved4)

# --- shortest_path_between: THE BUG - PE1 and PE3 aren't directly linked,
# the only route is via P1. Confirms the fix actually reconstructs it. ---
sub_path = shortest_path_between(PE1, PE3, "igp", by_id, sr_capable_ids)
assert sub_path == [PE1, P1, PE3], f"expected PE1->P1->PE3, got {sub_path}"
print("shortest_path_between (multi-hop via P1): OK", sub_path)

# --- shortest_path_between: destination itself is reachable even without
# Node-SID/SR-capability (dst is exempt from the transit-only pruning rule,
# same split cspf.go makes between "can originate/terminate here" and "can
# transit through here") ---
sub_path_to_non_sr_dst = shortest_path_between(PE1, P3_LEGACY, "igp", by_id, sr_capable_ids)
assert sub_path_to_non_sr_dst == [PE1, P2, P3_LEGACY], sub_path_to_non_sr_dst
print("shortest_path_between (non-SR destination still reachable): OK", sub_path_to_non_sr_dst)

# --- shortest_path_between: a non-SR-capable node must be pruned as a
# *transit* hop even when it would be the physically shorter route -
# synthetic topology: A-sr -- B-nonsr -- D-sr (direct, cheap) and
# A-sr -- C-sr -- D-sr (longer, but all-SR). The only valid route is via C. ---
def _synthetic_node(router_id, sr_capable):
    node = {"routerID": router_id, "links": []}
    if sr_capable:
        node["srgbBegin"], node["srgbEnd"] = 16000, 17000
        node["prefixes"] = [{"prefix": "10.9.9.9/32", "sidIndex": 1}]
    return node


def _synthetic_link(a, b, metric):
    a["links"].append({"remoteNode": b["routerID"], "metrics": [{"type": "METRIC_TYPE_IGP", "value": metric}]})
    b["links"].append({"remoteNode": a["routerID"], "metrics": [{"type": "METRIC_TYPE_IGP", "value": metric}]})


node_a = _synthetic_node("A", sr_capable=True)
node_b = _synthetic_node("B", sr_capable=False)
node_c = _synthetic_node("C", sr_capable=True)
node_d = _synthetic_node("D", sr_capable=True)
_synthetic_link(node_a, node_b, 1)
_synthetic_link(node_b, node_d, 1)
_synthetic_link(node_a, node_c, 10)
_synthetic_link(node_c, node_d, 1)
synthetic_by_id = {n["routerID"]: n for n in [node_a, node_b, node_c, node_d]}
synthetic_sr_ids = {"A", "C", "D"}

pruned_path = shortest_path_between("A", "D", "igp", synthetic_by_id, synthetic_sr_ids)
assert pruned_path == ["A", "C", "D"], f"expected the longer all-SR route via C, got {pruned_path}"
print("shortest_path_between (non-SR node correctly pruned as transit): OK", pruned_path)

# --- expand_to_physical_path: end-to-end for the single-segment case ---
expanded = expand_to_physical_path([PE1, PE3], "igp", by_id, sr_capable_ids)
assert expanded["path"] == [PE1, P1, PE3], expanded
assert expanded["approximate"] == []
print("expand_to_physical_path (reconstructs real PE1->P1->PE3 hop): OK", expanded)

# --- expand_to_physical_path: already-adjacent waypoints stay a direct hop ---
expanded2 = expand_to_physical_path([PE1, P2, PE2], "igp", by_id, sr_capable_ids)
assert expanded2["path"] == [PE1, P2, PE2], expanded2
print("expand_to_physical_path (already-adjacent waypoints unchanged): OK", expanded2)

# --- enrich_policies: end-to-end shape used by the API ---
enriched = enrich_policies(policies, ted)
assert len(enriched) == 3
byname = {p["policyName"]: p for p in enriched}
assert byname["mesh-pe1-pe2"]["path"] == [PE1, P2, PE2]
assert byname["mesh-pe1-pe2"]["pathEdges"] == [f"{PE1}__{P2}", f"{P2}__{PE2}"]
assert byname["mesh-pe1-pe2"]["peerAddr"] == "10.255.0.1"
# the actual regression: mesh-pe1-pe3's single node-SID segment must expand
# to the real PE1->P1->PE3 route, with a real edge for the frontend to
# highlight - not a phantom PE1-PE3 edge that doesn't exist in the topology.
assert byname["mesh-pe1-pe3"]["path"] == [PE1, P1, PE3]
assert byname["mesh-pe1-pe3"]["pathEdges"] == [f"{PE1}__{P1}", f"{P1}__{PE3}"]
assert byname["mesh-pe1-pe3"]["approximateHops"] == []
print("enrich_policies: OK")

print("\nAll topology.py smoke tests passed.")

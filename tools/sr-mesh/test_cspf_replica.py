#!/usr/bin/env python3
"""Parity tests for cspf_replica.py, ported 1:1 from pkg/cspf/cspf_test.go.

These exist to catch drift between this Python port and the real Go CSPF
implementation it mirrors. Run directly (stdlib-only, no pytest needed):

    python3 test_cspf_replica.py

If pkg/cspf/cspf.go's algorithm ever changes, port the corresponding Go test
case here too.
"""
from __future__ import annotations

from cspf_replica import CSPFError, spf


def sr_capable_node(router_id: str, sid_index: int) -> dict:
    """Mirrors cspf_test.go's srCapableNode: SRGB 16000-17000, label
    16000+sidIndex."""
    return {
        "routerID": router_id,
        "srgbBegin": 16000,
        "srgbEnd": 17000,
        "prefixes": [{"prefix": "10.0.0.1/32", "sidIndex": sid_index}],
        "links": [],
    }


def non_sr_capable_node(router_id: str) -> dict:
    """Mirrors cspf_test.go's nonSRCapableNode: no Prefix-SID."""
    return {"routerID": router_id, "srgbBegin": 0, "srgbEnd": 0, "prefixes": [], "links": []}


def link(a: dict, b: dict, metric: int) -> None:
    """Mirrors cspf_test.go's link(): bidirectional IGP-metric adjacency."""
    a["links"].append({"remoteNode": b["routerID"], "metrics": [{"type": "METRIC_TYPE_IGP", "value": metric}]})
    b["links"].append({"remoteNode": a["routerID"], "metrics": [{"type": "METRIC_TYPE_IGP", "value": metric}]})


def test_skips_non_sr_capable_neighbor() -> None:
    """Port of TestCSPF_SkipsNonSRCapableNeighbor: cheapest path transits a
    non-SR node B; CSPF must prune it and take the pricier all-SR path via C."""
    a = sr_capable_node("A", 0)
    b = non_sr_capable_node("B")
    c = sr_capable_node("C", 1)
    d = sr_capable_node("D", 2)

    link(a, b, 1)   # cheapest first hop, but B is not SR-capable
    link(b, d, 1)   # A->B->D costs 2 total -- would win if B were usable
    link(a, c, 10)
    link(c, d, 1)   # A->C->D costs 11 total -- the only valid all-SR path

    node_by_id = {"A": a, "B": b, "C": c, "D": d}
    segments = spf("A", "D", "igp", node_by_id)

    assert segments == [16001, 16002], f"want [C_sid, D_sid]=[16001, 16002], got {segments}"


def test_no_all_sr_path_returns_clean_error() -> None:
    """Port of TestCSPF_NoAllSRPath_ReturnsCleanError: the only route to D
    transits non-SR-capable B, with no alternative -- must fail cleanly."""
    a = sr_capable_node("A", 0)
    b = non_sr_capable_node("B")
    d = sr_capable_node("D", 1)

    link(a, b, 1)
    link(b, d, 1)  # the only route from A to D goes through non-SR-capable B

    node_by_id = {"A": a, "B": b, "D": d}

    try:
        spf("A", "D", "igp", node_by_id)
        raise AssertionError("expected CSPFError since no all-SR-MPLS path exists")
    except CSPFError as e:
        assert str(e) == "no SR-MPLS path found from A to D", f"unexpected message: {e}"


def test_source_without_node_sid_is_hard_error() -> None:
    """Port of TestCSPF_SourceWithoutNodeSID_IsHardError: unlike a transit
    neighbor, a source with no Node SID can't be pruned around."""
    a = non_sr_capable_node("A")
    d = sr_capable_node("D", 0)
    link(a, d, 1)

    node_by_id = {"A": a, "D": d}

    try:
        spf("A", "D", "igp", node_by_id)
        raise AssertionError("expected CSPFError since the source node has no Node SID")
    except CSPFError:
        pass


def test_metric_type_normalization_matches_real_ted_output() -> None:
    """Real `pola ted -j` output uses "METRIC_TYPE_IGP" (see
    test/scenario/show-ted/*/expected/*.json), not the bare "IGP" the stale
    README sample shows. Confirm the requested "igp" metric matches it."""
    a = sr_capable_node("A", 0)
    d = sr_capable_node("D", 1)
    a["links"].append({"remoteNode": "D", "metrics": [{"type": "METRIC_TYPE_IGP", "value": 5}]})
    d["links"].append({"remoteNode": "A", "metrics": [{"type": "METRIC_TYPE_IGP", "value": 5}]})

    segments = spf("A", "D", "igp", {"A": a, "D": d})
    assert segments == [16001], segments


def test_missing_requested_metric_on_link_is_hard_error() -> None:
    """Mirrors cspf.go: if a link doesn't carry the requested metric type at
    all, that's a hard failure for the whole computation, not a prune."""
    a = sr_capable_node("A", 0)
    d = sr_capable_node("D", 1)
    a["links"].append({"remoteNode": "D", "metrics": [{"type": "METRIC_TYPE_TE", "value": 5}]})
    d["links"].append({"remoteNode": "A", "metrics": [{"type": "METRIC_TYPE_TE", "value": 5}]})

    try:
        spf("A", "D", "igp", {"A": a, "D": d})
        raise AssertionError("expected CSPFError since the link has no igp metric")
    except CSPFError:
        pass


def main() -> None:
    tests = [
        test_skips_non_sr_capable_neighbor,
        test_no_all_sr_path_returns_clean_error,
        test_source_without_node_sid_is_hard_error,
        test_metric_type_normalization_matches_real_ted_output,
        test_missing_requested_metric_on_link_is_hard_error,
    ]
    failures = 0
    for test in tests:
        try:
            test()
            print(f"PASS  {test.__name__}")
        except AssertionError as e:
            failures += 1
            print(f"FAIL  {test.__name__}: {e}")
    print()
    if failures:
        print(f"{failures}/{len(tests)} test(s) failed")
        raise SystemExit(1)
    print(f"all {len(tests)} tests passed")


if __name__ == "__main__":
    main()

"""Synthetic TED/session/policy data, shaped exactly like `pola ted -j` /
`pola session -j` / `pola sr-policy list -j` output, for running and
demoing this tool without a live polad. Topology: a small ring (PE1-P1-P2-PE2)
with a couple of extra nodes hanging off it, so there's more than one path
between endpoints - similar in spirit to the real lab this was built against.
"""
from __future__ import annotations

SRGB_BEGIN = 16000
SRGB_END = 24000


def _node(router_id, hostname, sid_index, loopback, extra_prefixes=None):
    prefixes = [{"prefix": f"{loopback}/32", "sidIndex": sid_index}]
    prefixes.extend(extra_prefixes or [])
    return {
        "asn": 65000,
        "routerID": router_id,
        "isisAreaID": "490000",
        "hostname": hostname,
        "srgbBegin": SRGB_BEGIN,
        "srgbEnd": SRGB_END,
        "prefixes": prefixes,
        "links": [],
        "srv6SIDs": [],
    }


def _link(a, b, a_ip, b_ip, metric, adj_sid_a, adj_sid_b):
    a["links"].append({
        "adjSid": adj_sid_a, "localIP": a_ip, "remoteIP": b_ip,
        "metrics": [{"type": "METRIC_TYPE_IGP", "value": metric}],
        "remoteNode": b["routerID"],
    })
    b["links"].append({
        "adjSid": adj_sid_b, "localIP": b_ip, "remoteIP": a_ip,
        "metrics": [{"type": "METRIC_TYPE_IGP", "value": metric}],
        "remoteNode": a["routerID"],
    })


def build_mock_ted():
    pe1 = _node("0000.0000.0001", "PE1", 1, "10.255.0.1")
    p1 = _node("0000.0000.0002", "P1", 2, "10.255.0.2")
    p2 = _node("0000.0000.0003", "P2", 3, "10.255.0.3")
    pe2 = _node("0000.0000.0004", "PE2", 4, "10.255.0.4")
    pe3 = _node("0000.0000.0005", "PE3", 5, "10.255.0.5")
    p3_no_sr = {
        "asn": 65000, "routerID": "0000.0000.0006", "isisAreaID": "490000",
        "hostname": "P3-LEGACY", "srgbBegin": 0, "srgbEnd": 0,
        "prefixes": [{"prefix": "10.255.0.6/32"}], "links": [], "srv6SIDs": [],
    }

    _link(pe1, p1, "10.0.1.1", "10.0.1.2", 10, 24001, 24002)
    _link(p1, p2, "10.0.2.1", "10.0.2.2", 10, 24003, 24004)
    _link(p2, pe2, "10.0.3.1", "10.0.3.2", 10, 24005, 24006)
    _link(pe1, p2, "10.0.4.1", "10.0.4.2", 20, 24007, 24008)
    _link(p1, pe3, "10.0.5.1", "10.0.5.2", 15, 24009, 24010)
    _link(p2, p3_no_sr, "10.0.6.1", "10.0.6.2", 10, 24011, 24012)

    return {"ted": [pe1, p1, p2, pe2, pe3, p3_no_sr]}


def build_mock_sessions():
    return [
        {
            "Addr": "10.255.0.1",
            "State": "SESSION_STATE_UP",
            "Capabilities": [{"Type": "STATEFUL", "Detail": {"LSPUpdate": True, "Color": True}}],
            "IsSynced": True,
        },
        {
            "Addr": "10.255.0.4",
            "State": "SESSION_STATE_UP",
            "Capabilities": [{"Type": "STATEFUL", "Detail": {"LSPUpdate": True, "Color": True}}],
            "IsSynced": True,
        },
    ]


def build_mock_policies():
    return [
        {
            "peerAddr": "10.255.0.1",
            "srPolicies": [
                {
                    "plspId": 1,
                    "policyName": "mesh-pe1-pe2",
                    "segmentList": [{"sid": 16003}, {"sid": 16004}],
                    "srcAddr": "10.255.0.1",
                    "dstAddr": "10.255.0.4",
                    "srcRouterId": "0000.0000.0001",
                    "dstRouterId": "0000.0000.0004",
                    "color": 100,
                    "preference": 100,
                    "lspId": 3,
                    "state": "up",
                    "type": "dynamic",
                    "metric": "igp",
                },
                {
                    "plspId": 2,
                    "policyName": "mesh-pe1-pe3",
                    "segmentList": [{"sid": 16005}],
                    "srcAddr": "10.255.0.1",
                    "dstAddr": "10.255.0.5",
                    "srcRouterId": "0000.0000.0001",
                    "dstRouterId": "0000.0000.0005",
                    "color": 100,
                    "preference": 100,
                    "lspId": 1,
                    "state": "up",
                    "type": "dynamic",
                    "metric": "igp",
                },
            ],
        },
        {
            "peerAddr": "10.255.0.4",
            "srPolicies": [
                {
                    "plspId": 1,
                    "policyName": "mesh-pe2-pe1",
                    "segmentList": [{"sid": 16002}, {"sid": 16001}],
                    "srcAddr": "10.255.0.4",
                    "dstAddr": "10.255.0.1",
                    "srcRouterId": "0000.0000.0004",
                    "dstRouterId": "0000.0000.0001",
                    "color": 100,
                    "preference": 100,
                    "lspId": 2,
                    "state": "up",
                    "type": "dynamic",
                    "metric": "igp",
                },
            ],
        },
    ]

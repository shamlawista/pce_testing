// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package cspf

import (
	"net/netip"
	"testing"

	"github.com/nttcom/pola/pkg/table"
)

// srCapableNode returns an LsNode with a valid Node SID (SRGB 16000-17000,
// label 16000+sidIndex).
func srCapableNode(routerID string, sidIndex uint32) *table.LsNode {
	return &table.LsNode{
		RouterID:  routerID,
		SrgbBegin: 16000,
		SrgbEnd:   17000,
		Prefixes: []*table.LsPrefix{
			{Prefix: netip.MustParsePrefix("10.0.0.1/32"), SidIndex: sidIndex, HasSidIndex: true},
		},
	}
}

// nonSRCapableNode returns an LsNode with no Prefix-SID and no SRv6 SID -
// NodeSegment() on this node always errors, simulating a device without
// SR-MPLS enabled (e.g. no SRGB configured).
func nonSRCapableNode(routerID string) *table.LsNode {
	return &table.LsNode{RouterID: routerID}
}

// link creates bidirectional connectivity between a and b with the given
// IGP metric in both directions, mirroring how two BGP-LS-reported LsLink
// objects (one per direction) would appear in a real TED.
func link(a, b *table.LsNode, metric uint32) {
	a.Links = append(a.Links, &table.LsLink{
		LocalNode:  a,
		RemoteNode: b,
		Metrics:    []*table.Metric{table.NewMetric(table.IGPMetric, metric)},
	})
	b.Links = append(b.Links, &table.LsLink{
		LocalNode:  b,
		RemoteNode: a,
		Metrics:    []*table.Metric{table.NewMetric(table.IGPMetric, metric)},
	})
}

// TestCSPF_SkipsNonSRCapableNeighbor builds a network where the cheapest IGP
// path from A to D transits B, a neighbor with no Node SID. CSPF must prune B
// and find the more expensive but valid all-SR path through C, instead of
// aborting with "node doesn't have a Node SID".
func TestCSPF_SkipsNonSRCapableNeighbor(t *testing.T) {
	a := srCapableNode("A", 0)
	b := nonSRCapableNode("B")
	c := srCapableNode("C", 1)
	d := srCapableNode("D", 2)

	link(a, b, 1) // cheapest first hop, but B is not SR-capable
	link(b, d, 1) // A->B->D costs 2 total - would be the SPF choice if B were usable
	link(a, c, 10)
	link(c, d, 1) // A->C->D costs 11 total - the only valid all-SR path

	network := map[string]*table.LsNode{"A": a, "B": b, "C": c, "D": d}

	segmentList, err := spf("A", "D", table.IGPMetric, network, nil)
	if err != nil {
		t.Fatalf("spf returned an error, want a valid path avoiding the non-SR node B: %v", err)
	}
	if len(segmentList) != 2 {
		t.Fatalf("segment list: got %d segments, want 2 (C, D): %v", len(segmentList), segmentList)
	}
}

// TestCSPF_NoAllSRPath_ReturnsCleanError builds a network where the only path
// from A to D transits B, a non-SR-capable node, with no alternative route.
// CSPF must fail with a clear "no path found" error, not the underlying
// "node doesn't have a Node SID" error from NodeSegment().
func TestCSPF_NoAllSRPath_ReturnsCleanError(t *testing.T) {
	a := srCapableNode("A", 0)
	b := nonSRCapableNode("B")
	d := srCapableNode("D", 1)

	link(a, b, 1)
	link(b, d, 1) // the only route from A to D goes through non-SR-capable B

	network := map[string]*table.LsNode{"A": a, "B": b, "D": d}

	_, err := spf("A", "D", table.IGPMetric, network, nil)
	if err == nil {
		t.Fatal("expected an error since no all-SR-MPLS path exists, got nil")
	}
	if err.Error() != "no SR-MPLS path found from A to D" {
		t.Errorf("error message: got %q, want a clean \"no SR-MPLS path found\" message, not the underlying NodeSegment error", err.Error())
	}
}

// TestCSPF_SourceWithoutNodeSID_IsHardError confirms that, unlike a transit
// neighbor, a source node without a Node SID is still a hard failure: you
// cannot originate an SR path from a non-SR-capable node.
func TestCSPF_SourceWithoutNodeSID_IsHardError(t *testing.T) {
	a := nonSRCapableNode("A")
	d := srCapableNode("D", 0)
	link(a, d, 1)

	network := map[string]*table.LsNode{"A": a, "D": d}

	_, err := spf("A", "D", table.IGPMetric, network, nil)
	if err == nil {
		t.Fatal("expected an error since the source node has no Node SID, got nil")
	}
}

// TestCSPF_ExcludesSpecifiedNode mirrors TestCSPF_SkipsNonSRCapableNeighbor's
// topology exactly, but B is SR-capable and excluded explicitly instead of
// lacking a Node SID - confirming exclusion prunes a transit candidate the
// same way non-SR-capability already does, rather than only ever ruling out
// nodes CSPF would have rejected anyway.
func TestCSPF_ExcludesSpecifiedNode(t *testing.T) {
	a := srCapableNode("A", 0)
	b := srCapableNode("B", 3) // SR-capable, but explicitly excluded below
	c := srCapableNode("C", 1)
	d := srCapableNode("D", 2)

	link(a, b, 1) // cheapest first hop, but B is excluded
	link(b, d, 1) // A->B->D costs 2 total - would be the SPF choice if B weren't excluded
	link(a, c, 10)
	link(c, d, 1) // A->C->D costs 11 total - the only path avoiding B

	network := map[string]*table.LsNode{"A": a, "B": b, "C": c, "D": d}
	ted := &table.LsTED{Nodes: network}

	segmentList, err := CSPF("A", "D", table.IGPMetric, ted, []string{"B"})
	if err != nil {
		t.Fatalf("CSPF returned an error, want a valid path avoiding excluded node B: %v", err)
	}
	if len(segmentList) != 2 {
		t.Fatalf("segment list: got %d segments, want 2 (C, D): %v", len(segmentList), segmentList)
	}
	bsSID, _ := b.NodeSegment() // srgbBegin(16000) + sidIndex(3) = "16003"
	for _, seg := range segmentList {
		if seg.SidString() == bsSID.SidString() {
			t.Errorf("excluded node B's own SID (%s) must not appear in the computed path: %v", bsSID.SidString(), segmentList)
		}
	}
}

// TestCSPF_NoPathAfterExclusion_ReturnsCleanError mirrors
// TestCSPF_NoAllSRPath_ReturnsCleanError: the only route from A to D transits
// B, which is SR-capable but excluded, so no valid path remains. Must fail
// with the same clean "no path found" error used for the non-SR-neighbor
// case - excluding a node "over-prunes" the graph, but the resulting failure
// mode is identical from the caller's point of view.
func TestCSPF_NoPathAfterExclusion_ReturnsCleanError(t *testing.T) {
	a := srCapableNode("A", 0)
	b := srCapableNode("B", 3)
	d := srCapableNode("D", 1)

	link(a, b, 1)
	link(b, d, 1) // the only route from A to D goes through B

	network := map[string]*table.LsNode{"A": a, "B": b, "D": d}
	ted := &table.LsTED{Nodes: network}

	_, err := CSPF("A", "D", table.IGPMetric, ted, []string{"B"})
	if err == nil {
		t.Fatal("expected an error since excluding B leaves no path, got nil")
	}
	if err.Error() != "no SR-MPLS path found from A to D" {
		t.Errorf("error message: got %q, want the same clean \"no SR-MPLS path found\" message as the non-SR-neighbor case", err.Error())
	}
}

// TestCSPF_SourceExcluded_IsHardError confirms excluding a path's own source
// is rejected up front with a clear, distinct error - not silently treated
// as "no path found" (which would look identical to a genuine topology gap).
func TestCSPF_SourceExcluded_IsHardError(t *testing.T) {
	a := srCapableNode("A", 0)
	d := srCapableNode("D", 1)
	link(a, d, 1)

	ted := &table.LsTED{Nodes: map[string]*table.LsNode{"A": a, "D": d}}

	_, err := CSPF("A", "D", table.IGPMetric, ted, []string{"A"})
	if err == nil {
		t.Fatal("expected an error since the source itself is excluded, got nil")
	}
	if err.Error() != "cannot exclude router A: it is the path's own source" {
		t.Errorf("error message: got %q, want a clear source-excluded error, not a generic no-path result", err.Error())
	}
}

// TestCSPF_DestinationExcluded_IsHardError is the destination-side mirror of
// TestCSPF_SourceExcluded_IsHardError.
func TestCSPF_DestinationExcluded_IsHardError(t *testing.T) {
	a := srCapableNode("A", 0)
	d := srCapableNode("D", 1)
	link(a, d, 1)

	ted := &table.LsTED{Nodes: map[string]*table.LsNode{"A": a, "D": d}}

	_, err := CSPF("A", "D", table.IGPMetric, ted, []string{"D"})
	if err == nil {
		t.Fatal("expected an error since the destination itself is excluded, got nil")
	}
	if err.Error() != "cannot exclude router D: it is the path's own destination" {
		t.Errorf("error message: got %q, want a clear destination-excluded error, not a generic no-path result", err.Error())
	}
}

// TestCSPFWithLooseSourceRouting_WaypointExcluded_IsHardError confirms an
// explicit waypoint conflicting with the exclusion list is rejected with a
// message distinct from the plain source/destination case, since the
// operator needs to know it's the waypoint (not srcRouterID/dstRouterID)
// causing the conflict.
func TestCSPFWithLooseSourceRouting_WaypointExcluded_IsHardError(t *testing.T) {
	a := srCapableNode("A", 0)
	b := srCapableNode("B", 3)
	d := srCapableNode("D", 1)
	link(a, b, 1)
	link(b, d, 1)

	ted := &table.LsTED{Nodes: map[string]*table.LsNode{"A": a, "B": b, "D": d}}

	_, err := CSPFWithLooseSourceRouting("A", "D", []table.Waypoint{{RouterID: "B"}}, table.IGPMetric, ted, []string{"B"})
	if err == nil {
		t.Fatal("expected an error since waypoint B is also excluded, got nil")
	}
	if err.Error() != "cannot exclude router B: it is an explicit waypoint of this path" {
		t.Errorf("error message: got %q, want a clear waypoint-conflict error", err.Error())
	}
}

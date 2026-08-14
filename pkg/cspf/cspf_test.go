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

	segmentList, err := spf("A", "D", table.IGPMetric, network)
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

	_, err := spf("A", "D", table.IGPMetric, network)
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

	_, err := spf("A", "D", table.IGPMetric, network)
	if err == nil {
		t.Fatal("expected an error since the source node has no Node SID, got nil")
	}
}

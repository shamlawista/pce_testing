// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package cspf

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/nttcom/pola/pkg/table"
)

type node struct {
	id          string
	calculated  bool
	cost        uint32
	prevNode    string
	nodeSegment table.Segment
}

func newNode(id string, cost uint32, nodeSeg table.Segment) *node {
	return &node{
		id:          id,
		cost:        cost,
		nodeSegment: nodeSeg,
	}
}

// CSPF computes the shortest (per metric) all-SR-MPLS-capable path from
// srcRouterID to dstRouterID, excluding every router ID in excluded from
// consideration entirely - as if those nodes (and every link through them)
// didn't exist in the graph. excluded may be nil or empty for no exclusion.
//
// Excluding the source or destination itself is rejected up front with a
// clear error (RFC 5521 exclusion semantics don't define "compute a path
// that both starts/ends at and excludes the same node") - this is
// deliberately a hard error, not folded into the generic "no path found"
// case, so the two are never confused with each other.
func CSPF(srcRouterID string, dstRouterID string, metric table.MetricType, ted *table.LsTED, excluded []string) ([]table.Segment, error) {
	if err := validateExclusion(srcRouterID, dstRouterID, excluded); err != nil {
		return nil, err
	}

	network := ted.Nodes
	// TODO: update network information according to constraints
	segmentList, err := spf(srcRouterID, dstRouterID, metric, network, excludedSet(excluded))
	if err != nil {
		return nil, err
	}

	return segmentList, nil
}

// validateExclusion rejects excluding a request's own source, destination,
// or (for loose source routing) any explicit waypoint - excluding a node
// that the path is required to pass through is a contradiction in the
// request itself, not a topology condition, so it must never be reported as
// a plain "no path found".
func validateExclusion(srcRouterID, dstRouterID string, excluded []string) error {
	for _, id := range excluded {
		switch id {
		case srcRouterID:
			return fmt.Errorf("cannot exclude router %s: it is the path's own source", id)
		case dstRouterID:
			return fmt.Errorf("cannot exclude router %s: it is the path's own destination", id)
		}
	}
	return nil
}

// excludedSet converts an excluded-router-ID list into the set form spf's
// graph walk checks against. A nil/empty input is a valid, cheap "exclude
// nothing".
func excludedSet(excluded []string) map[string]struct{} {
	if len(excluded) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(excluded))
	for _, id := range excluded {
		set[id] = struct{}{}
	}
	return set
}

// CSPFWithLooseSourceRouting computes a path with optional waypoints using loose source routing.
func CSPFWithLooseSourceRouting(
	src, dst string,
	waypoints []table.Waypoint,
	metric table.MetricType,
	ted *table.LsTED,
	excluded []string,
) ([]table.Segment, error) {
	// Checked separately from the src/dst-of-each-section validation
	// CSPF() already does below: an *explicit* waypoint conflicting with
	// exclusion is a different, clearer error than "own source"/"own
	// destination" would be if checked only per-section.
	for _, wp := range waypoints {
		if slices.Contains(excluded, wp.RouterID) {
			return nil, fmt.Errorf("cannot exclude router %s: it is an explicit waypoint of this path", wp.RouterID)
		}
	}

	fullList := []table.Segment{}
	prev := src

	// Append destination as a pseudo-waypoint without mutating the input slice
	allWaypoints := append(append([]table.Waypoint{}, waypoints...), table.Waypoint{RouterID: dst})

	for _, wp := range allWaypoints {
		sectionSegs, seg, err := buildSectionSegments(prev, wp, metric, ted, fullList, excluded)
		if err != nil {
			return nil, err
		}
		fullList = append(fullList, sectionSegs...)
		fullList = appendIfNotDuplicate(fullList, seg)
		prev = wp.RouterID
	}

	return fullList, nil
}

// buildSectionSegments calculates CSPF to waypoint and builds the waypoint segment.
func buildSectionSegments(prev string, wp table.Waypoint, metric table.MetricType, ted *table.LsTED, fullList []table.Segment, excluded []string) ([]table.Segment, table.Segment, error) {
	// Compute CSPF from prev → waypoint
	sectionSegs, err := CSPF(prev, wp.RouterID, metric, ted, excluded)
	if err != nil {
		return nil, nil, fmt.Errorf("CSPF failed between %s and %s: %w", prev, wp.RouterID, err)
	}

	// Remove first segment if it duplicates the last segment of the previous sections
	sectionSegs = removeDuplicateFirst(fullList, sectionSegs)

	// Lookup the node from TED
	node, ok := ted.Nodes[wp.RouterID]
	if !ok {
		return nil, nil, fmt.Errorf("waypoint router %s not found in TED", wp.RouterID)
	}

	// Build the segment (SRv6 or SR-MPLS)
	seg, err := buildWaypointSegment(node, wp.SID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build segment for waypoint %s: %w", wp.RouterID, err)
	}

	return sectionSegs, seg, nil
}

// buildWaypointSegment builds a Segment for a waypoint using the node and optional explicit SID.
func buildWaypointSegment(node *table.LsNode, explicitSID string) (table.Segment, error) {
	if explicitSID != "" {
		addr, err := netip.ParseAddr(explicitSID)
		if err != nil {
			return nil, fmt.Errorf("invalid explicit SID %q: %w", explicitSID, err)
		}
		return table.NewSegmentSRv6WithNodeInfo(addr, node)
	}
	return node.NodeSegment()
}

// removeDuplicateFirst removes the first segment of section if it equals the last of fullList.
func removeDuplicateFirst(fullList []table.Segment, section []table.Segment) []table.Segment {
	if len(fullList) > 0 && len(section) > 0 && table.SegmentsEqual(fullList[len(fullList)-1], section[0]) {
		return section[1:]
	}
	return section
}

// appendIfNotDuplicate appends a segment to the list if it is not equal to the last segment.
func appendIfNotDuplicate(list []table.Segment, seg table.Segment) []table.Segment {
	if len(list) == 0 || !table.SegmentsEqual(list[len(list)-1], seg) {
		list = append(list, seg)
	}
	return list
}

func spf(srcRouterID string, dstRouterID string, metricType table.MetricType, network map[string]*table.LsNode, excluded map[string]struct{}) ([]table.Segment, error) {
	calculatingNodes, err := initNodeMap(srcRouterID, network)
	if err != nil {
		return nil, err
	}

	// Keep calculating the shortest path until the destination node is reached.
	for {
		calcNodeID, err := nextNode(calculatingNodes)
		if err != nil {
			// The frontier is exhausted without ever reaching dstRouterID: every
			// remaining candidate was either already calculated or pruned (e.g.
			// non-SR-capable or explicitly excluded neighbors in
			// updateNeighborCosts). That means no all-SR-MPLS path avoiding the
			// excluded set exists to the destination.
			return nil, fmt.Errorf("no SR-MPLS path found from %s to %s", srcRouterID, dstRouterID)
		}
		if calcNodeID == dstRouterID {
			break
		}

		if err := updateNeighborCosts(calcNodeID, calculatingNodes, network, metricType, excluded); err != nil {
			return nil, err
		}

		calculatingNodes[calcNodeID].calculated = true
	}

	return buildSegmentListFromPath(srcRouterID, dstRouterID, calculatingNodes), nil
}

// initNodeMap initializes the map of nodes used for SPF calculation.
func initNodeMap(srcRouterID string, network map[string]*table.LsNode) (map[string]*node, error) {
	startNodeSeg, err := network[srcRouterID].NodeSegment()
	if err != nil {
		return nil, err
	}
	startNode := newNode(srcRouterID, 0, startNodeSeg)
	startNode.calculated = false
	return map[string]*node{srcRouterID: startNode}, nil
}

// updateNeighborCosts updates costs for neighbors of the given node in SPF calculation.
func updateNeighborCosts(calcNodeID string, calculatingNodes map[string]*node, network map[string]*table.LsNode, metricType table.MetricType, excluded map[string]struct{}) error {
	for _, link := range network[calcNodeID].Links {
		if _, isExcluded := excluded[link.RemoteNode.RouterID]; isExcluded {
			// Treat an excluded node as absent from the graph entirely, same
			// as a non-SR-capable neighbor below - it may not even be needed
			// for the shortest remaining SR path.
			continue
		}

		metric, err := link.Metric(metricType)
		if err != nil {
			return err
		}

		if remoteNode, exists := calculatingNodes[link.RemoteNode.RouterID]; exists {
			if calculatingNodes[calcNodeID].cost+metric < remoteNode.cost {
				remoteNode.cost = calculatingNodes[calcNodeID].cost + metric
				remoteNode.prevNode = calcNodeID
			}
		} else {
			// A neighbor without a Node SID isn't usable as an SR-MPLS transit
			// hop. Prune it from the graph rather than aborting the whole
			// computation - it may not even be needed for the shortest SR path.
			remoteNodeSeg, err := link.RemoteNode.NodeSegment()
			if err != nil {
				continue
			}
			remoteNode := newNode(link.RemoteNode.RouterID, calculatingNodes[calcNodeID].cost+metric, remoteNodeSeg)
			remoteNode.prevNode = calcNodeID
			calculatingNodes[link.RemoteNode.RouterID] = remoteNode
		}
	}
	return nil
}

// buildSegmentListFromPath builds the segment list from SPF results.
func buildSegmentListFromPath(srcRouterID, dstRouterID string, calculatingNodes map[string]*node) []table.Segment {
	segmentList := []table.Segment{}
	for pathNode := calculatingNodes[dstRouterID]; pathNode.id != srcRouterID; pathNode = calculatingNodes[pathNode.prevNode] {
		segmentList = append(segmentList, pathNode.nodeSegment)
	}

	// Reverse the segment list to get correct order from src → dst
	for i, j := 0, len(segmentList)-1; i < j; i, j = i+1, j-1 {
		segmentList[i], segmentList[j] = segmentList[j], segmentList[i]
	}

	return segmentList
}

// nextNode returns the ID of the next node to calculate.
func nextNode(calculatingNodes map[string]*node) (string, error) {
	nextNodeID := ""
	for nodeID, node := range calculatingNodes {
		if node.calculated {
			continue
		}
		if nextNodeID == "" || calculatingNodes[nextNodeID].cost > node.cost {
			nextNodeID = nodeID
		}
	}
	if nextNodeID == "" {
		return "", errors.New("next node not found")
	}
	return nextNodeID, nil
}

// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package main

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nttcom/pola/api/pola/v1"
)

// TestBuildExplicitPolicy_RejectsExclude confirms buildExplicitPolicy itself
// rejects `exclude` for type: explicit, rather than only ever being exercised
// transitively through addSRPolicyWithRouterID.
func TestBuildExplicitPolicy_RejectsExclude(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:        "explicit",
			SegmentList: []Segment{{SID: "16003"}},
			Exclude:     []Exclude{{RouterID: "0000.0aff.0002"}},
		},
	}

	_, _, _, _, _, err := buildExplicitPolicy(input, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only meaningful for `type: dynamic`")
}

func TestBuildExplicitPolicy_NoExcludeSucceeds(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:        "explicit",
			SegmentList: []Segment{{SID: "16003"}},
		},
	}

	policyType, _, segments, _, exclude, err := buildExplicitPolicy(input, "")
	require.NoError(t, err)
	assert.Equal(t, pb.SRPolicyType_SR_POLICY_TYPE_EXPLICIT, policyType)
	assert.Len(t, segments, 1)
	assert.Empty(t, exclude)
}

// TestBuildDynamicPolicy_ConvertsExcludeToRouterIDs confirms the YAML
// []Exclude{RouterID} list is flattened into the plain []string the gRPC
// layer and CSPF expect, in the router-ID resolution order given.
func TestBuildDynamicPolicy_ConvertsExcludeToRouterIDs(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:   "dynamic",
			Metric: "igp",
			Exclude: []Exclude{
				{RouterID: "0000.0aff.0002"},
				{RouterID: "0000.0aff.0003"},
			},
		},
	}

	policyType, metric, segments, waypoints, exclude, err := buildDynamicPolicy(input, "")
	require.NoError(t, err)
	assert.Equal(t, pb.SRPolicyType_SR_POLICY_TYPE_DYNAMIC, policyType)
	assert.Equal(t, pb.MetricType_METRIC_TYPE_IGP, metric)
	assert.Nil(t, segments)
	assert.Nil(t, waypoints)
	assert.Equal(t, []string{"0000.0aff.0002", "0000.0aff.0003"}, exclude)
}

func TestBuildDynamicPolicy_NoExcludeIsEmpty(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:   "dynamic",
			Metric: "te",
		},
	}

	_, _, _, _, exclude, err := buildDynamicPolicy(input, "")
	require.NoError(t, err)
	assert.Empty(t, exclude)
}

// TestAddSRPolicyWithEndpointAddr_RejectsExclude confirms the srcAddr/dstAddr
// form (which bypasses CSPF entirely) rejects `exclude` before ever reaching
// the gRPC client, matching the metric/waypoints checks it already has.
func TestAddSRPolicyWithEndpointAddr_RejectsExclude(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			PCEPSessionAddr: netip.MustParseAddr("192.0.2.1"),
			SrcAddr:         netip.MustParseAddr("192.0.2.1"),
			DstAddr:         netip.MustParseAddr("192.0.2.2"),
			Color:           100,
			SegmentList:     []Segment{{SID: "16003"}},
			Exclude:         []Exclude{{RouterID: "0000.0aff.0002"}},
		},
	}

	err := addSRPolicyWithEndpointAddr(input, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require a dynamic path")
}

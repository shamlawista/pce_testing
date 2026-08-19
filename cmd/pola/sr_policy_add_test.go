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

	_, _, _, _, _, _, err := buildExplicitPolicy(input, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only meaningful for `type: dynamic`")
}

// TestBuildExplicitPolicy_RejectsExcludeSid confirms the sid-based exclude
// form is rejected for type: explicit exactly like the routerID form.
func TestBuildExplicitPolicy_RejectsExcludeSid(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:        "explicit",
			SegmentList: []Segment{{SID: "16003"}},
			Exclude:     []Exclude{{SID: "16002"}},
		},
	}

	_, _, _, _, _, _, err := buildExplicitPolicy(input, "")
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

	policyType, _, segments, _, excludeRouterIDs, excludeSIDs, err := buildExplicitPolicy(input, "")
	require.NoError(t, err)
	assert.Equal(t, pb.SRPolicyType_SR_POLICY_TYPE_EXPLICIT, policyType)
	assert.Len(t, segments, 1)
	assert.Empty(t, excludeRouterIDs)
	assert.Empty(t, excludeSIDs)
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

	policyType, metric, segments, waypoints, excludeRouterIDs, excludeSIDs, err := buildDynamicPolicy(input, "")
	require.NoError(t, err)
	assert.Equal(t, pb.SRPolicyType_SR_POLICY_TYPE_DYNAMIC, policyType)
	assert.Equal(t, pb.MetricType_METRIC_TYPE_IGP, metric)
	assert.Nil(t, segments)
	assert.Nil(t, waypoints)
	assert.Equal(t, []string{"0000.0aff.0002", "0000.0aff.0003"}, excludeRouterIDs)
	assert.Empty(t, excludeSIDs)
}

// TestBuildDynamicPolicy_ConvertsExcludeToSIDs is the SID-form sibling of
// TestBuildDynamicPolicy_ConvertsExcludeToRouterIDs.
func TestBuildDynamicPolicy_ConvertsExcludeToSIDs(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:   "dynamic",
			Metric: "igp",
			Exclude: []Exclude{
				{SID: "16002"},
				{SID: "16003"},
			},
		},
	}

	_, _, _, _, excludeRouterIDs, excludeSIDs, err := buildDynamicPolicy(input, "")
	require.NoError(t, err)
	assert.Empty(t, excludeRouterIDs)
	assert.Equal(t, []string{"16002", "16003"}, excludeSIDs)
}

// TestBuildDynamicPolicy_CombinesRouterIDAndSidExcludes confirms an exclude
// list mixing both forms splits each entry into the right slice, in order.
func TestBuildDynamicPolicy_CombinesRouterIDAndSidExcludes(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:   "dynamic",
			Metric: "igp",
			Exclude: []Exclude{
				{RouterID: "0000.0aff.0002"},
				{SID: "16003"},
			},
		},
	}

	_, _, _, _, excludeRouterIDs, excludeSIDs, err := buildDynamicPolicy(input, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"0000.0aff.0002"}, excludeRouterIDs)
	assert.Equal(t, []string{"16003"}, excludeSIDs)
}

func TestBuildDynamicPolicy_ExcludeEntryWithBothRouterIDAndSidIsRejected(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:   "dynamic",
			Metric: "igp",
			Exclude: []Exclude{
				{RouterID: "0000.0aff.0002", SID: "16003"},
			},
		},
	}

	_, _, _, _, _, _, err := buildDynamicPolicy(input, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both routerID")
}

func TestBuildDynamicPolicy_ExcludeEntryWithNeitherRouterIDNorSidIsRejected(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:    "dynamic",
			Metric:  "igp",
			Exclude: []Exclude{{}},
		},
	}

	_, _, _, _, _, _, err := buildDynamicPolicy(input, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neither routerID nor sid")
}

func TestBuildDynamicPolicy_NoExcludeIsEmpty(t *testing.T) {
	input := InputFormat{
		SRPolicy: SRPolicy{
			Type:   "dynamic",
			Metric: "te",
		},
	}

	_, _, _, _, excludeRouterIDs, excludeSIDs, err := buildDynamicPolicy(input, "")
	require.NoError(t, err)
	assert.Empty(t, excludeRouterIDs)
	assert.Empty(t, excludeSIDs)
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

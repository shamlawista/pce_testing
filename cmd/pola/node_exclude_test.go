// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeArgFlags_RouterIDOnly(t *testing.T) {
	cmd := newNodeExcludeAddCmd()
	require.NoError(t, cmd.Flags().Set("routerID", "0000.0aff.0002"))

	routerID, sid, err := nodeArgFlags(cmd)
	require.NoError(t, err)
	assert.Equal(t, "0000.0aff.0002", routerID)
	assert.Empty(t, sid)
}

func TestNodeArgFlags_SidOnly(t *testing.T) {
	cmd := newNodeExcludeAddCmd()
	require.NoError(t, cmd.Flags().Set("sid", "16002"))

	routerID, sid, err := nodeArgFlags(cmd)
	require.NoError(t, err)
	assert.Empty(t, routerID)
	assert.Equal(t, "16002", sid)
}

func TestNodeArgFlags_RejectsBoth(t *testing.T) {
	cmd := newNodeExcludeAddCmd()
	require.NoError(t, cmd.Flags().Set("routerID", "0000.0aff.0002"))
	require.NoError(t, cmd.Flags().Set("sid", "16002"))

	_, _, err := nodeArgFlags(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestNodeArgFlags_RejectsNeither(t *testing.T) {
	cmd := newNodeExcludeAddCmd()

	_, _, err := nodeArgFlags(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

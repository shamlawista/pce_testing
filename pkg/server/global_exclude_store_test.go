// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobalExcludeStore_AddListRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newGlobalExcludeStore(filepath.Join(dir, "global-exclude.json"))

	assert.Empty(t, s.list())

	require.NoError(t, s.add("router-a"))
	require.NoError(t, s.add("router-b"))
	assert.Equal(t, []string{"router-a", "router-b"}, s.list())
}

func TestGlobalExcludeStore_AddIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := newGlobalExcludeStore(filepath.Join(dir, "global-exclude.json"))

	require.NoError(t, s.add("router-a"))
	require.NoError(t, s.add("router-a"))
	assert.Equal(t, []string{"router-a"}, s.list())
}

func TestGlobalExcludeStore_Remove(t *testing.T) {
	dir := t.TempDir()
	s := newGlobalExcludeStore(filepath.Join(dir, "global-exclude.json"))

	require.NoError(t, s.add("router-a"))
	require.NoError(t, s.add("router-b"))
	require.NoError(t, s.remove("router-a"))
	assert.Equal(t, []string{"router-b"}, s.list())
}

func TestGlobalExcludeStore_RemoveAbsentIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global-exclude.json")
	s := newGlobalExcludeStore(path)

	require.NoError(t, s.remove("does-not-exist"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file to be created for a no-op remove (err=%v)", err)
	}
}

// TestGlobalExcludeStore_PersistsAcrossFreshLoad is the "survives a polad
// restart" proof, mirroring TestIntentStore_SavePersistsAcrossFreshLoad.
func TestGlobalExcludeStore_PersistsAcrossFreshLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global-exclude.json")

	s := newGlobalExcludeStore(path)
	require.NoError(t, s.add("router-a"))
	require.NoError(t, s.add("router-b"))

	reloaded, err := loadGlobalExcludeStore(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"router-a", "router-b"}, reloaded.list())
}

// TestGlobalExcludeStore_RemovalPersistsAcrossFreshLoad confirms a remove is
// just as durable as an add - the whole point of the feature is that
// turning a node's exclusion off is immediate and doesn't linger after a
// restart.
func TestGlobalExcludeStore_RemovalPersistsAcrossFreshLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global-exclude.json")

	s := newGlobalExcludeStore(path)
	require.NoError(t, s.add("router-a"))
	require.NoError(t, s.remove("router-a"))

	reloaded, err := loadGlobalExcludeStore(path)
	require.NoError(t, err)
	assert.Empty(t, reloaded.list())
}

func TestLoadGlobalExcludeStore_MissingFileIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	s, err := loadGlobalExcludeStore(path)
	require.NoError(t, err)
	assert.Empty(t, s.list())
}

func TestLoadGlobalExcludeStore_CorruptFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global-exclude.json")
	require.NoError(t, os.WriteFile(path, []byte("not valid json"), 0o600))

	_, err := loadGlobalExcludeStore(path)
	require.Error(t, err)
}

func TestGlobalExcludeStore_SaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global-exclude.json")
	s := newGlobalExcludeStore(path)

	require.NoError(t, s.add("router-a"))
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected the temp file to be renamed away, but it still exists (err=%v)", err)
	}
}

func TestMergeGlobalExclude_UnionsAndDedupes(t *testing.T) {
	got := mergeGlobalExclude([]string{"a"}, []string{"a", "b"}, nil)
	assert.ElementsMatch(t, []string{"a", "b"}, got)
}

func TestMergeGlobalExclude_NoGlobalExcludeReturnsPolicyExcludeUnchanged(t *testing.T) {
	policyExclude := []string{"a"}
	got := mergeGlobalExclude(policyExclude, nil, nil)
	assert.Equal(t, policyExclude, got)
}

// TestMergeGlobalExclude_SkipsProtectedEndpoints confirms the endpoint-
// conflict design decision: a global exclusion naming this specific
// policy's own src/dst (or waypoint) is silently dropped from the merge,
// rather than propagating into the CSPF call where it would hard-error.
func TestMergeGlobalExclude_SkipsProtectedEndpoints(t *testing.T) {
	got := mergeGlobalExclude(nil, []string{"src-router", "transit-router"}, []string{"src-router"})
	assert.Equal(t, []string{"transit-router"}, got)
}

// TestMergeGlobalExclude_PolicyExcludeNotFilteredByProtected confirms the
// endpoint-skip only applies to the global set - a policy's own explicit
// exclude naming its own src/dst is a different, pre-existing hard-error
// path (cspf.validateExclusion), which mergeGlobalExclude must not mask.
func TestMergeGlobalExclude_PolicyExcludeNotFilteredByProtected(t *testing.T) {
	got := mergeGlobalExclude([]string{"src-router"}, nil, []string{"src-router"})
	assert.Equal(t, []string{"src-router"}, got)
}

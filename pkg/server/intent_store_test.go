// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nttcom/pola/pkg/table"
)

func TestIntentStore_SaveLookupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newIntentStore(filepath.Join(dir, "intents.json"))
	addr := netip.MustParseAddr("10.0.0.1")

	if _, _, _, ok := s.lookup(addr, "unknown"); ok {
		t.Fatal("expected no entry before any save")
	}

	if err := s.save(addr, "policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	polType, metric, _, ok := s.lookup(addr, "policy1")
	if !ok {
		t.Fatal("expected to find the saved entry")
	}
	if polType != table.PolicyTypeDynamic || metric != table.TEMetric {
		t.Errorf("got Type=%q Metric=%v, want Type=%q Metric=%v", polType, metric, table.PolicyTypeDynamic, table.TEMetric)
	}

	// Different address, same name - must not match.
	if _, _, _, ok := s.lookup(netip.MustParseAddr("10.0.0.2"), "policy1"); ok {
		t.Error("expected no match for a different peer address")
	}
}

// TestIntentStore_SavePersistsAcrossFreshLoad is the actual "survives a
// polad restart" proof: a brand-new intentStore instance, loaded from the
// same path, must see what an earlier instance saved.
func TestIntentStore_SavePersistsAcrossFreshLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	addr := netip.MustParseAddr("10.0.0.1")

	s := newIntentStore(path)
	if err := s.save(addr, "mesh-a-b", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	reloaded, err := loadIntentStore(path)
	if err != nil {
		t.Fatalf("loadIntentStore failed: %v", err)
	}
	polType, metric, _, ok := reloaded.lookup(addr, "mesh-a-b")
	if !ok {
		t.Fatal("expected intent to survive a fresh load, but it was not found")
	}
	if polType != table.PolicyTypeDynamic || metric != table.TEMetric {
		t.Errorf("got Type=%q Metric=%v, want Type=%q Metric=%v", polType, metric, table.PolicyTypeDynamic, table.TEMetric)
	}
}

// TestIntentStore_SaveUnchangedDoesNotRewrite validates the hot-path safety
// fix: save() must skip the disk rewrite when the value is already what's
// stored, since SendPCUpdate (and therefore save) is called on every
// TED-triggered reoptimize and every spontaneous PCC re-report, where
// Type/Metric almost never actually change.
func TestIntentStore_SaveUnchangedDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	s := newIntentStore(path)
	addr := netip.MustParseAddr("10.0.0.1")

	if err := s.save(addr, "policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("first save failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist after first save: %v", err)
	}

	// Remove the file, then save the exact same value again. If save()
	// correctly no-ops on an unchanged value, the file must NOT reappear.
	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}
	if err := s.save(addr, "policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("second (unchanged) save failed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no rewrite for an unchanged save, but the file exists (err=%v)", err)
	}
}

func TestIntentStore_DeleteRemovesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	addr := netip.MustParseAddr("10.0.0.1")

	s := newIntentStore(path)
	if err := s.save(addr, "policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if err := s.delete(addr, "policy1"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if _, _, _, ok := s.lookup(addr, "policy1"); ok {
		t.Error("expected entry to be gone after delete")
	}

	reloaded, err := loadIntentStore(path)
	if err != nil {
		t.Fatalf("loadIntentStore failed: %v", err)
	}
	if _, _, _, ok := reloaded.lookup(addr, "policy1"); ok {
		t.Error("expected deletion to persist across a fresh load")
	}
}

func TestIntentStore_DeleteAbsentKeyDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	s := newIntentStore(path)

	if err := s.delete(netip.MustParseAddr("10.0.0.1"), "does-not-exist"); err != nil {
		t.Fatalf("delete of an absent key failed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file to be created for a no-op delete (err=%v)", err)
	}
}

func TestLoadIntentStore_MissingFileIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	s, err := loadIntentStore(path)
	if err != nil {
		t.Fatalf("expected no error for a missing file, got: %v", err)
	}
	if _, _, _, ok := s.lookup(netip.MustParseAddr("10.0.0.1"), "anything"); ok {
		t.Error("expected an empty store")
	}
}

func TestLoadIntentStore_CorruptFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0o600); err != nil {
		t.Fatalf("failed to write test fixture: %v", err)
	}

	if _, err := loadIntentStore(path); err == nil {
		t.Fatal("expected an error for a corrupt file")
	}
}

// TestIntentStore_MetricRoundTrip directly regression-tests the exact JSON
// risk this design exists to avoid: table.MetricType has a MarshalJSON but
// no UnmarshalJSON, so persistedIntent stores metrics as plain strings
// instead - this proves every metric value, including the zero value,
// survives a save+reload unchanged.
func TestIntentStore_MetricRoundTrip(t *testing.T) {
	cases := []table.MetricType{
		table.UnspecifiedMetric,
		table.IGPMetric,
		table.TEMetric,
		table.DelayMetric,
		table.HopcountMetric,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	s := newIntentStore(path)
	addr := netip.MustParseAddr("10.0.0.1")

	for i, metric := range cases {
		name := fmt.Sprintf("policy-%d", i)
		if err := s.save(addr, name, table.PolicyTypeDynamic, metric, nil); err != nil {
			t.Fatalf("save failed for metric %v: %v", metric, err)
		}
	}

	loaded, err := loadIntentStore(path)
	if err != nil {
		t.Fatalf("loadIntentStore failed: %v", err)
	}
	for i, metric := range cases {
		name := fmt.Sprintf("policy-%d", i)
		_, gotMetric, _, ok := loaded.lookup(addr, name)
		if !ok {
			t.Fatalf("policy %s not found after reload", name)
		}
		if gotMetric != metric {
			t.Errorf("metric round trip for %v: got %v", metric, gotMetric)
		}
	}
}

// TestIntentStore_ExcludeRoundTrip mirrors TestIntentStore_SavePersistsAcrossFreshLoad
// but for Exclude specifically - the field most likely to be forgotten if a
// future change touches Type/Metric persistence without also carrying
// Exclude through, since it was added after those two.
func TestIntentStore_ExcludeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	addr := netip.MustParseAddr("10.0.0.1")

	s := newIntentStore(path)
	exclude := []string{"router-a", "router-b"}
	if err := s.save(addr, "avoid-migration-node", table.PolicyTypeDynamic, table.IGPMetric, exclude); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	reloaded, err := loadIntentStore(path)
	if err != nil {
		t.Fatalf("loadIntentStore failed: %v", err)
	}
	_, _, gotExclude, ok := reloaded.lookup(addr, "avoid-migration-node")
	if !ok {
		t.Fatal("expected intent to survive a fresh load, but it was not found")
	}
	if !slices.Equal(gotExclude, exclude) {
		t.Errorf("got Exclude=%v, want %v", gotExclude, exclude)
	}

	// A save with no exclusion at all must round-trip as empty, not carry
	// over stale data from a previous save under the same key.
	if err := s.save(addr, "no-exclusion", table.PolicyTypeDynamic, table.IGPMetric, nil); err != nil {
		t.Fatalf("save (nil exclude) failed: %v", err)
	}
	_, _, gotExclude, ok = s.lookup(addr, "no-exclusion")
	if !ok || len(gotExclude) != 0 {
		t.Errorf("got Exclude=%v ok=%v, want empty/nil and ok=true", gotExclude, ok)
	}
}

func TestIntentStore_SamePolicyNameDifferentPeersDontLeak(t *testing.T) {
	dir := t.TempDir()
	s := newIntentStore(filepath.Join(dir, "intents.json"))
	addr1 := netip.MustParseAddr("10.0.0.1")
	addr2 := netip.MustParseAddr("10.0.0.2")

	if err := s.save(addr1, "shared-name", table.PolicyTypeDynamic, table.IGPMetric, nil); err != nil {
		t.Fatalf("save addr1 failed: %v", err)
	}
	if err := s.save(addr2, "shared-name", table.PolicyTypeExplicit, table.UnspecifiedMetric, nil); err != nil {
		t.Fatalf("save addr2 failed: %v", err)
	}

	polType1, metric1, _, ok := s.lookup(addr1, "shared-name")
	if !ok || polType1 != table.PolicyTypeDynamic || metric1 != table.IGPMetric {
		t.Errorf("addr1: got Type=%q Metric=%v ok=%v, want Dynamic/IGP/true", polType1, metric1, ok)
	}
	polType2, _, _, ok := s.lookup(addr2, "shared-name")
	if !ok || polType2 != table.PolicyTypeExplicit {
		t.Errorf("addr2: got Type=%q ok=%v, want Explicit/true", polType2, ok)
	}
}

func TestIntentStore_SaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intents.json")
	s := newIntentStore(path)

	if err := s.save(netip.MustParseAddr("10.0.0.1"), "policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected the temp file to be renamed away, but it still exists (err=%v)", err)
	}
}

func TestIntentStore_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	s := newIntentStore(filepath.Join(dir, "intents.json"))
	addr := netip.MustParseAddr("10.0.0.1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = s.save(addr, "policy1", table.PolicyTypeDynamic, table.TEMetric, nil)
			_ = s.delete(addr, "policy2")
		}
	}()

	for i := 0; i < 50; i++ {
		s.lookup(addr, "policy1")
	}
	<-done
}

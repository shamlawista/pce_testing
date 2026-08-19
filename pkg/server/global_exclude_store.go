// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"
)

const globalExcludeStoreVersion = 1

// globalExcludeStoreFile is the on-disk envelope, versioned for the same
// reason intentStoreFile is.
type globalExcludeStoreFile struct {
	Version   int      `json:"version"`
	RouterIDs []string `json:"routerIDs"`
}

// globalExcludeStore persists a set of router IDs to keep out of CSPF
// consideration for every dynamically-computed policy server-wide,
// independent of any single policy's own intent (see intentStore/Exclude).
// Deliberately never merged into a policy's own persisted Exclude - see
// Server.effectiveExclude - so removing a node here immediately un-excludes
// it from every future reoptimization, without touching any policy's own
// stored intent.
type globalExcludeStore struct {
	mu    sync.Mutex
	path  string
	nodes map[string]struct{}
}

// newGlobalExcludeStore returns an empty store that persists to path.
func newGlobalExcludeStore(path string) *globalExcludeStore {
	return &globalExcludeStore{
		path:  path,
		nodes: make(map[string]struct{}),
	}
}

// loadGlobalExcludeStore reads path into a new store. A missing file is
// treated as a legitimate empty store (first run), not an error.
func loadGlobalExcludeStore(path string) (*globalExcludeStore, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newGlobalExcludeStore(path), nil
		}
		return nil, fmt.Errorf("failed to read global exclude store %q: %w", path, err)
	}

	var file globalExcludeStoreFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("failed to parse global exclude store %q: %w", path, err)
	}

	nodes := make(map[string]struct{}, len(file.RouterIDs))
	for _, id := range file.RouterIDs {
		nodes[id] = struct{}{}
	}
	return &globalExcludeStore{path: path, nodes: nodes}, nil
}

// add adds routerID to the global exclusion set and persists it, unless it
// was already present - mirrors intentStore.save's unchanged-value no-op.
func (s *globalExcludeStore) add(routerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.nodes[routerID]; ok {
		return nil
	}
	s.nodes[routerID] = struct{}{}
	return s.writeLocked()
}

// remove removes routerID from the global exclusion set and persists it, if
// it was present.
func (s *globalExcludeStore) remove(routerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.nodes[routerID]; !ok {
		return nil
	}
	delete(s.nodes, routerID)
	return s.writeLocked()
}

// list returns a sorted snapshot of the current global exclusion set, safe
// for concurrent use.
func (s *globalExcludeStore) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// mergeGlobalExclude unions globalExclude into policyExclude for a single
// CSPF call, silently omitting any global entry that names one of protected
// (this specific policy's own src/dst, or explicit waypoints for a request
// being computed). The global set is a blanket "avoid this node everywhere"
// setting, so it must not break an unrelated policy for which the same node
// happens to be a required endpoint - that stays this policy's own explicit
// exclude's problem to validate, not the global set's.
//
// The result is only ever used for the CSPF call itself, never persisted:
// callers must keep passing policyExclude (not this result) to anything
// that stores intent, or a global exclusion would get baked into individual
// policies and outlive being removed from the global set.
func mergeGlobalExclude(policyExclude, globalExclude, protected []string) []string {
	if len(globalExclude) == 0 {
		return policyExclude
	}

	merged := slices.Clone(policyExclude)
	for _, g := range globalExclude {
		if slices.Contains(protected, g) || slices.Contains(merged, g) {
			continue
		}
		merged = append(merged, g)
	}
	return merged
}

// writeLocked rewrites the whole store to disk atomically. Callers must
// hold s.mu. The temp file must live in the same directory as s.path, same
// reasoning as intentStore.writeLocked.
func (s *globalExcludeStore) writeLocked() error {
	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	raw, err := json.MarshalIndent(globalExcludeStoreFile{Version: globalExcludeStoreVersion, RouterIDs: ids}, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal global exclude store: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("failed to write global exclude store temp file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("failed to rename global exclude store temp file into place: %w", err)
	}
	return nil
}

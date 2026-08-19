// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"sync"

	"github.com/nttcom/pola/pkg/table"
)

const intentStoreVersion = 1

// persistedIntent is the on-disk representation of an SR policy's intent
// (Type/Metric/Exclude). Type/Metric are stored as plain strings rather than
// table.PolicyType/table.MetricType directly: MetricType has a MarshalJSON
// (via DisplayString(), e.g. "te") but no UnmarshalJSON, so round-tripping
// it through encoding/json directly would silently fail to decode.
type persistedIntent struct {
	Type    string   `json:"type"`
	Metric  string   `json:"metric"`
	Exclude []string `json:"exclude,omitempty"`
}

// intentStoreFile is the on-disk envelope. Versioned since this is the
// first on-disk artifact this codebase ships - cheap insurance against an
// awkward migration if the shape ever needs to change later.
type intentStoreFile struct {
	Version int                                   `json:"version"`
	Peers   map[string]map[string]persistedIntent `json:"peers"`
}

// intentStore persists SR policy intent (Type/Metric), keyed by (peer
// session address, policy name), so it survives a polad restart - unlike
// the in-memory, SRP-ID-keyed intent used for near-term PCUpd/PCRpt
// correlation (see srPolicyIntent), which is ephemeral by design and can't
// survive a restart or even a plain PCEP resync (state-sync PCRpts report
// SRP-ID 0, never matching a remembered request).
type intentStore struct {
	mu   sync.Mutex
	path string
	data map[string]map[string]persistedIntent // peerAddr.String() -> policyName -> intent
}

// newIntentStore returns an empty store that persists to path.
func newIntentStore(path string) *intentStore {
	return &intentStore{
		path: path,
		data: make(map[string]map[string]persistedIntent),
	}
}

// loadIntentStore reads path into a new store. A missing file is treated
// as a legitimate empty store (first run), not an error; any other read or
// parse failure is returned so the caller can decide how to degrade.
func loadIntentStore(path string) (*intentStore, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newIntentStore(path), nil
		}
		return nil, fmt.Errorf("failed to read intent store %q: %w", path, err)
	}

	var file intentStoreFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("failed to parse intent store %q: %w", path, err)
	}

	data := file.Peers
	if data == nil {
		data = make(map[string]map[string]persistedIntent)
	}
	return &intentStore{path: path, data: data}, nil
}

// save records the intent for (peerAddr, name) and persists it, unless the
// value is already what's stored - a no-op skip that matters because save
// is called on hot paths (every reoptimize-triggered PCUpd, every
// spontaneous PCRpt for an already-known policy), where Type/Metric almost
// never actually change and a full-file rewrite would otherwise happen far
// more often than the "infrequent mutation" assumption this design relies
// on to avoid needing a real embedded KV store.
func (s *intentStore) save(peerAddr netip.Addr, name string, polType table.PolicyType, metric table.MetricType, exclude []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := persistedIntent{Type: string(polType), Metric: metricTypeToString(metric), Exclude: exclude}
	peer := peerAddr.String()
	if existing, ok := s.data[peer][name]; ok && existing.equal(next) {
		return nil
	}

	if s.data[peer] == nil {
		s.data[peer] = make(map[string]persistedIntent)
	}
	s.data[peer][name] = next
	return s.writeLocked()
}

// equal reports whether two persistedIntent values hold the same data -
// []string makes persistedIntent non-comparable with ==, unlike before
// Exclude existed.
func (i persistedIntent) equal(other persistedIntent) bool {
	return i.Type == other.Type && i.Metric == other.Metric && slices.Equal(i.Exclude, other.Exclude)
}

// lookup returns the persisted intent for (peerAddr, name), if any.
func (s *intentStore) lookup(peerAddr netip.Addr, name string) (table.PolicyType, table.MetricType, []string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	intent, ok := s.data[peerAddr.String()][name]
	if !ok {
		return "", table.UnspecifiedMetric, nil, false
	}
	return table.PolicyType(intent.Type), metricTypeFromString(intent.Metric), intent.Exclude, true
}

// delete removes the persisted intent for (peerAddr, name), if present.
// Deliberately never called on a mere session disconnect/reconnect - only
// on an explicit policy delete - since surviving disconnects is the whole
// point of this store.
func (s *intentStore) delete(peerAddr netip.Addr, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer := peerAddr.String()
	if _, ok := s.data[peer][name]; !ok {
		return nil
	}

	delete(s.data[peer], name)
	if len(s.data[peer]) == 0 {
		delete(s.data, peer)
	}
	return s.writeLocked()
}

// writeLocked rewrites the whole store to disk atomically. Callers must
// hold s.mu. The temp file must live in the same directory as s.path for
// os.Rename to be atomic - a different directory can cross filesystems and
// either fail outright or silently degrade to non-atomic on some platforms.
func (s *intentStore) writeLocked() error {
	raw, err := json.MarshalIndent(intentStoreFile{Version: intentStoreVersion, Peers: s.data}, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal intent store: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("failed to write intent store temp file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("failed to rename intent store temp file into place: %w", err)
	}
	return nil
}

// metricTypeToString renders m for on-disk storage.
func metricTypeToString(m table.MetricType) string {
	return m.DisplayString()
}

// metricTypeFromString parses the on-disk metric string back into a
// table.MetricType. An empty or unrecognized value maps to
// table.UnspecifiedMetric rather than erroring - a corrupt/unknown metric
// string on one entry shouldn't be fatal to loading the rest of the store.
func metricTypeFromString(s string) table.MetricType {
	switch s {
	case "igp":
		return table.IGPMetric
	case "te":
		return table.TEMetric
	case "delay":
		return table.DelayMetric
	case "hopcount":
		return table.HopcountMetric
	default:
		return table.UnspecifiedMetric
	}
}

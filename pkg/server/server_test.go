// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"testing"

	"go.uber.org/zap"

	"github.com/nttcom/pola/pkg/packet/pcep"
	"github.com/nttcom/pola/pkg/table"
)

// Regression test: strconv.Atoi accepts negative numbers, which used to
// wrap around to a valid uint16 (e.g. -1 -> 65535) instead of being rejected.
func TestServer_Serve_NegativePortRejected(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	if err := s.Serve("127.0.0.1", "-1", false); err == nil {
		t.Fatal("expected error for negative port")
	}
}

func TestServer_Serve_PortOutOfRange(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	if err := s.Serve("127.0.0.1", "70000", false); err == nil {
		t.Fatal("expected error for out-of-range port")
	}
}

func TestPropagateTED_UpdatesEverySession(t *testing.T) {
	ss1 := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss2 := NewSession(2, netip.MustParseAddr("10.0.255.2"), nil, zap.NewNop(), nil, 0)
	ss3 := NewSession(3, netip.MustParseAddr("10.0.255.3"), nil, zap.NewNop(), nil, 0)
	s := &Server{sessionList: []*Session{ss1, ss2, ss3}}

	newTED := &table.LsTED{Nodes: map[string]*table.LsNode{}}
	s.propagateTED(newTED)

	for i, ss := range []*Session{ss1, ss2, ss3} {
		if got := ss.TED(); got != newTED {
			t.Errorf("session %d: TED() = %p, want %p", i, got, newTED)
		}
	}
}

func TestPropagateTED_EmptySessionList(t *testing.T) {
	s := &Server{}
	s.propagateTED(&table.LsTED{Nodes: map[string]*table.LsNode{}})
}

// newTestDynamicSession builds a synced-or-not session carrying one dynamic
// SR Policy (installed segments [16002, 16003], src=10.255.0.1/dst=10.255.0.2)
// wired to a live loopback conn, for the Server-level reoptimize tests below.
func newTestDynamicSession(t *testing.T, sessionID uint8, synced bool) (ss *Session, clientConn *net.TCPConn) {
	t.Helper()
	serverConn, client := newTCPConnPair(t)
	ss = NewSession(sessionID, netip.MustParseAddr(fmt.Sprintf("10.0.255.%d", sessionID)), serverConn, zap.NewNop(), nil, 0)
	if synced {
		ss.setSynced()
	}

	sr := newTestStateReport(t, 1, 7)
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}
	return ss, client
}

func TestServerReoptimizeDynamicPolicies_SkipsUnsyncedSessions(t *testing.T) {
	unsyncedSS, unsyncedClient := newTestDynamicSession(t, 1, false)
	syncedSS, syncedClient := newTestDynamicSession(t, 2, true)
	s := &Server{sessionList: []*Session{unsyncedSS, syncedSS}}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 99)
	linkTEDNodes(src, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	s.reoptimizeDynamicPolicies(ted, zap.NewNop())

	assertNothingSent(t, unsyncedClient)
	got := decodePCUpdSegments(t, syncedClient)
	want := []table.Segment{table.NewSegmentSRMPLS(16099)}
	if !slices.EqualFunc(got, want, table.SegmentsEqual) {
		t.Errorf("synced session PCUpd segments = %v, want %v", got, want)
	}
}

func TestServerReoptimizeDynamicPolicies_NoSessions(t *testing.T) {
	s := &Server{}
	s.reoptimizeDynamicPolicies(&table.LsTED{Nodes: map[string]*table.LsNode{}}, zap.NewNop())
}

func TestServerReoptimizeDynamicPolicies_AggregatesAcrossSessions(t *testing.T) {
	ss1, client1 := newTestDynamicSession(t, 1, true)
	ss2, client2 := newTestDynamicSession(t, 2, true)
	s := &Server{sessionList: []*Session{ss1, ss2}}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 99)
	linkTEDNodes(src, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	s.reoptimizeDynamicPolicies(ted, zap.NewNop())

	want := []table.Segment{table.NewSegmentSRMPLS(16099)}
	for i, client := range []*net.TCPConn{client1, client2} {
		got := decodePCUpdSegments(t, client)
		if !slices.EqualFunc(got, want, table.SegmentsEqual) {
			t.Errorf("session %d PCUpd segments = %v, want %v", i, got, want)
		}
	}
}

func TestReoptimizeMu_TryLockPreventsOverlap(t *testing.T) {
	s := &Server{}
	if !s.reoptimizeMu.TryLock() {
		t.Fatal("expected first TryLock to succeed")
	}
	if s.reoptimizeMu.TryLock() {
		t.Fatal("expected second TryLock to fail while the first lock is held")
	}
	s.reoptimizeMu.Unlock()
	if !s.reoptimizeMu.TryLock() {
		t.Fatal("expected TryLock to succeed again after Unlock")
	}
	s.reoptimizeMu.Unlock()
}

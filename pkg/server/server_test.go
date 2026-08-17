// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"net/netip"
	"testing"

	"go.uber.org/zap"

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

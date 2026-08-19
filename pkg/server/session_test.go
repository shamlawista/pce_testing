// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	pb "github.com/nttcom/pola/api/pola/v1"
	"github.com/nttcom/pola/pkg/packet/pcep"
	"github.com/nttcom/pola/pkg/table"
)

// newTCPConnPair returns a connected TCP connection pair over loopback.
func newTCPConnPair(t *testing.T) (server, client *net.TCPConn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("failed to close listener: %v", err)
		}
	})

	serverCh := make(chan *net.TCPConn, 1)
	errCh := make(chan error, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		serverCh <- conn.(*net.TCPConn)
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	select {
	case server := <-serverCh:
		return server, clientConn.(*net.TCPConn)
	case err := <-errCh:
		t.Fatalf("failed to accept connection: %v", err)
		return nil, clientConn.(*net.TCPConn)
	}
}

// newTestStateReport builds a PCRpt state report for an SR-MPLS policy with an explicit path.
func newTestStateReport(t *testing.T, plspID uint32, srpID uint32) *pcep.StateReport {
	t.Helper()

	sr, err := pcep.NewStateReport()
	if err != nil {
		t.Fatalf("failed to create state report: %v", err)
	}

	sr.SrpObject.SrpID = srpID
	sr.LSPObject.PlspID = plspID
	sr.LSPObject.Name = "pe01-policy1"
	sr.LSPObject.SrcAddr = netip.MustParseAddr("10.255.0.1")
	sr.LSPObject.DstAddr = netip.MustParseAddr("10.255.0.2")
	sr.LSPObject.OFlag = 0x02

	for _, sid := range []uint32{16002, 16003} {
		subobj, err := pcep.NewSREroSubobject(table.NewSegmentSRMPLS(sid))
		if err != nil {
			t.Fatalf("failed to create SR ERO subobject: %v", err)
		}
		sr.EroObject.EroSubobjects = append(sr.EroObject.EroSubobjects, subobj)
	}

	return sr
}

// A PCC may report an SR Policy with SRP-ID 0 even when TED is unavailable.
func TestHandleStateReportWithoutTED(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	sr := newTestStateReport(t, 1, 0)

	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	policy, found := ss.SearchSRPolicy(sr.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy reported by the PCC was not registered")
	}
	if policy.Name != sr.LSPObject.Name {
		t.Errorf("policy name: got %q, want %q", policy.Name, sr.LSPObject.Name)
	}
	if len(policy.SegmentList) != 2 {
		t.Errorf("segment list length: got %d, want 2", len(policy.SegmentList))
	}
}

func TestSRPolicies_SnapshotSegmentListIsIndependent(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	sr := newTestStateReport(t, 1, 0)

	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	policies := ss.SRPolicies()
	if len(policies) != 1 || len(policies[0].SegmentList) == 0 {
		t.Fatalf("expected one SR Policy with a non-empty segment list, got %+v", policies)
	}

	want := policies[0].SegmentList[0]
	policies[0].SegmentList[0] = table.NewSegmentSRMPLS(99999)

	got, found := ss.SearchSRPolicy(sr.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy reported by the PCC was not registered")
	}
	if !reflect.DeepEqual(got.SegmentList[0], want) {
		t.Errorf("mutating the snapshot's SegmentList changed the session's SR Policy: got %v, want %v", got.SegmentList[0], want)
	}
}

func TestSRPolicies_SnapshotSRv6StructureIsIndependent(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)

	srv6Seg := table.NewSegmentSRv6(netip.MustParseAddr("2001:db8:1005::"))
	srv6Seg.Structure = table.SIDStructureBytes{32, 16, 0, 80}
	ss.srPolicies = append(ss.srPolicies, table.NewSRPolicy(1, "pe01-policy1", []table.Segment{srv6Seg}, netip.MustParseAddr("10.255.0.1"), netip.MustParseAddr("10.255.0.2"), 0, 0, 0, table.PolicyUp))

	policies := ss.SRPolicies()
	if len(policies) != 1 || len(policies[0].SegmentList) == 0 {
		t.Fatalf("expected one SR Policy with a non-empty segment list, got %+v", policies)
	}

	snapshotSeg, ok := policies[0].SegmentList[0].(table.SegmentSRv6)
	if !ok {
		t.Fatalf("segment type: got %T, want table.SegmentSRv6", policies[0].SegmentList[0])
	}
	snapshotSeg.Structure[0] = 99

	got, found := ss.SearchSRPolicy(1)
	if !found {
		t.Fatal("SR Policy was not registered")
	}
	gotSeg, ok := got.SegmentList[0].(table.SegmentSRv6)
	if !ok {
		t.Fatalf("segment type: got %T, want table.SegmentSRv6", got.SegmentList[0])
	}
	if !reflect.DeepEqual(gotSeg.Structure, table.SIDStructureBytes{32, 16, 0, 80}) {
		t.Errorf("mutating the snapshot's SegmentSRv6.Structure changed the session's SR Policy: got %v", gotSeg.Structure)
	}
}

// SearchPlspIDByName must match on name alone. Both policies here share the
// same (zeroed) Color and DstAddr - the exact situation a PCC that never
// echoes color back (e.g. Nokia SR OS) produces for every policy it reports -
// so a Color/DstAddr-keyed lookup would be unable to tell them apart.
func TestSearchPlspIDByName(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)

	dst := netip.MustParseAddr("213.119.192.225")
	ss.srPolicies = append(ss.srPolicies,
		table.NewSRPolicy(371, "clean-test-1786637173", nil, netip.MustParseAddr("213.119.192.12"), dst, 0, 0, 0, table.PolicyUp),
		table.NewSRPolicy(384, "clean-test-2-nai", nil, netip.MustParseAddr("213.119.192.12"), dst, 0, 0, 0, table.PolicyUp),
	)

	id, found := ss.SearchPlspIDByName("clean-test-2-nai")
	if !found {
		t.Fatal("expected to find clean-test-2-nai")
	}
	if id != 384 {
		t.Errorf("PlspID: got %d, want 384", id)
	}

	if _, found := ss.SearchPlspIDByName("does-not-exist"); found {
		t.Error("expected no match for a name that was never registered")
	}
}

func TestSRPolicyIntent_AttachedOnCreationBySRPID(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	sr := newTestStateReport(t, 1, 7)

	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)

	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	policy, found := ss.SearchSRPolicy(sr.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy reported by the PCC was not registered")
	}
	if policy.Type != table.PolicyTypeDynamic {
		t.Errorf("policy type: got %q, want %q", policy.Type, table.PolicyTypeDynamic)
	}
	if policy.Metric != table.TEMetric {
		t.Errorf("policy metric: got %v, want %v", policy.Metric, table.TEMetric)
	}
}

func TestSRPolicyIntent_AttachedOnUpdateBySRPID(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)

	ss.rememberSRPolicyIntent(1, table.PolicyTypeExplicit, table.UnspecifiedMetric, nil)
	sr := newTestStateReport(t, 1, 1)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	policy, found := ss.SearchSRPolicy(sr.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy reported by the PCC was not registered")
	}
	if policy.Type != table.PolicyTypeExplicit || policy.Metric != table.UnspecifiedMetric {
		t.Fatalf("initial policy intent: got (%q, %v), want (%q, %v)", policy.Type, policy.Metric, table.PolicyTypeExplicit, table.UnspecifiedMetric)
	}

	// A PCRpt for the same PLSP-ID takes the update path and must pick up the new intent.
	ss.rememberSRPolicyIntent(2, table.PolicyTypeDynamic, table.TEMetric, nil)
	sr2 := newTestStateReport(t, 1, 2)
	if err := ss.handleStateReport(sr2, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	policy, found = ss.SearchSRPolicy(sr2.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy reported by the PCC was not registered")
	}
	if policy.Type != table.PolicyTypeDynamic {
		t.Errorf("policy type after update: got %q, want %q", policy.Type, table.PolicyTypeDynamic)
	}
	if policy.Metric != table.TEMetric {
		t.Errorf("policy metric after update: got %v, want %v", policy.Metric, table.TEMetric)
	}
}

func TestSRPolicyIntent_UnknownWhenNeverRemembered(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	sr := newTestStateReport(t, 1, 0)

	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	policy, found := ss.SearchSRPolicy(sr.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy reported by the PCC was not registered")
	}
	if policy.Type != "" {
		t.Errorf("policy type: got %q, want unset", policy.Type)
	}
	if policy.Metric != table.UnspecifiedMetric {
		t.Errorf("policy metric: got %v, want UnspecifiedMetric", policy.Metric)
	}
}

func TestSRPolicyIntent_IndependentPerSRPID(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)

	ss.rememberSRPolicyIntent(1, table.PolicyTypeExplicit, table.UnspecifiedMetric, nil)
	ss.rememberSRPolicyIntent(2, table.PolicyTypeDynamic, table.TEMetric, nil)

	// SRP-ID 2's PCRpt arrives first and must only consume intent 2.
	srB := newTestStateReport(t, 20, 2)
	if err := ss.handleStateReport(srB, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}
	policyB, found := ss.SearchSRPolicy(srB.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy for SRP-ID 2 was not registered")
	}
	if policyB.Type != table.PolicyTypeDynamic || policyB.Metric != table.TEMetric {
		t.Errorf("policy for SRP-ID 2: got (%q, %v), want (%q, %v)", policyB.Type, policyB.Metric, table.PolicyTypeDynamic, table.TEMetric)
	}
	ss.srPolicyIntentsMu.Lock()
	_, ok := ss.srPolicyIntents[1]
	ss.srPolicyIntentsMu.Unlock()
	if !ok {
		t.Error("intent for SRP-ID 1 must survive consuming SRP-ID 2's intent")
	}

	// SRP-ID 1's PCRpt arrives second and must consume only intent 1.
	srA := newTestStateReport(t, 10, 1)
	if err := ss.handleStateReport(srA, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}
	policyA, found := ss.SearchSRPolicy(srA.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy for SRP-ID 1 was not registered")
	}
	if policyA.Type != table.PolicyTypeExplicit || policyA.Metric != table.UnspecifiedMetric {
		t.Errorf("policy for SRP-ID 1: got (%q, %v), want (%q, %v)", policyA.Type, policyA.Metric, table.PolicyTypeExplicit, table.UnspecifiedMetric)
	}
}

func TestSRPolicyIntent_UnsolicitedPCRptDoesNotConsume(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.rememberSRPolicyIntent(1, table.PolicyTypeDynamic, table.TEMetric, nil)

	sr := newTestStateReport(t, 1, 0)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	policy, found := ss.SearchSRPolicy(sr.LSPObject.PlspID)
	if !found {
		t.Fatal("SR Policy reported by the PCC was not registered")
	}
	if policy.Type != "" {
		t.Errorf("policy type: got %q, want unset (unsolicited PCRpt must not consume an intent)", policy.Type)
	}

	if _, ok := ss.takeSRPolicyIntent(1); !ok {
		t.Error("intent for SRP-ID 1 must remain after an unrelated unsolicited PCRpt")
	}
}

func TestSRPolicyIntent_ClearedOnRFlagDelete(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)

	// Create the policy unsolicited so its creation does not consume the intent below.
	sr := newTestStateReport(t, 1, 0)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	ss.rememberSRPolicyIntent(5, table.PolicyTypeDynamic, table.TEMetric, nil)

	del := newTestStateReport(t, 1, 5)
	del.LSPObject.RFlag = true
	if err := ss.handleStateReport(del, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	if _, found := ss.SearchSRPolicy(del.LSPObject.PlspID); found {
		t.Error("SR Policy is still registered after being reported as removed")
	}
	if _, ok := ss.takeSRPolicyIntent(5); ok {
		t.Error("intent for SRP-ID 5 was not removed after an R-Flag PCRpt")
	}
}

func TestHandlePCErr_ForgetsReportedSRPIDIntents(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.rememberSRPolicyIntent(1, table.PolicyTypeDynamic, table.TEMetric, nil)
	ss.rememberSRPolicyIntent(2, table.PolicyTypeExplicit, table.UnspecifiedMetric, nil)

	pcerrMessage, err := pcep.NewPCErrMessage(1, 1, nil)
	if err != nil {
		t.Fatalf("failed to create PCErr message: %v", err)
	}
	pcerrMessage.SRPs = []*pcep.SrpObject{{SrpID: 1}}

	ss.handlePCErr(pcerrMessage)

	if _, ok := ss.takeSRPolicyIntent(1); ok {
		t.Error("intent for SRP-ID 1 was not removed after a PCErr reporting it")
	}
	if _, ok := ss.takeSRPolicyIntent(2); !ok {
		t.Error("intent for SRP-ID 2 must survive a PCErr that does not report it")
	}
}

// A failed PCEP send must not leave an intent waiting for a PCRpt that will never arrive.
func TestSendSRPolicyRequest_ForgetsIntentOnSendFailure(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.isSynced = true
	wantSRPID := ss.srpIDHead

	// Close the PCEP-side connection so the send inside sendSRPolicyRequest fails.
	if err := server.Close(); err != nil {
		t.Fatalf("failed to close server connection: %v", err)
	}

	pce := &Server{sessionList: []*Session{ss}}
	apiServer := &APIServer{pce: pce, logger: zap.NewNop()}

	dstAddr := netip.MustParseAddr("10.255.0.2")
	req := &pb.CreateSRPolicyRequest{
		SrPolicy: &pb.SRPolicy{
			PcepSessionAddr: ss.peerAddr.AsSlice(),
			DstAddr:         dstAddr.AsSlice(),
			Color:           100,
			PolicyName:      "test-policy",
			Type:            pb.SRPolicyType_SR_POLICY_TYPE_EXPLICIT,
		},
		DisablePathCompute: true,
	}

	if err := sendSRPolicyRequest(apiServer, req, nil, nil, netip.MustParseAddr("10.255.0.1"), dstAddr, true); err == nil {
		t.Fatal("expected sendSRPolicyRequest to fail once the connection is closed")
	}

	if _, ok := ss.takeSRPolicyIntent(wantSRPID); ok {
		t.Error("srPolicyIntents entry was not removed after a failed send")
	}
}

// Closing a session must clear its remembered SR Policy intents.
func TestCloseSession_ClearsSRPolicyIntents(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.rememberSRPolicyIntent(1, table.PolicyTypeDynamic, table.TEMetric, nil)

	s := &Server{sessionList: []*Session{ss}, logger: zap.NewNop()}
	s.closeSession(ss)

	if len(ss.srPolicyIntents) != 0 {
		t.Errorf("srPolicyIntents was not cleared on session close: %+v", ss.srPolicyIntents)
	}
}

// TestCloseSession_DoesNotTouchIntentStore proves a mere disconnect never
// deletes persisted intent - surviving disconnects (including a polad
// restart) is the entire point of the durable intent store, unlike the
// ephemeral srPolicyIntents map, which closeSession does clear above. This
// is the single most important behavioral guarantee of the whole feature
// and the easiest thing for a future change to accidentally regress.
func TestCloseSession_DoesNotTouchIntentStore(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))
	if err := ss.intentStore.save(ss.peerAddr, "policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("failed to seed intent store: %v", err)
	}

	s := &Server{sessionList: []*Session{ss}, logger: zap.NewNop()}
	s.closeSession(ss)

	if _, _, _, ok := ss.intentStore.lookup(ss.peerAddr, "policy1"); !ok {
		t.Error("expected persisted intent to survive a session close")
	}
}

// Concurrent SendPCUpdate/SendPCInitiate calls must never allocate the same SRP-ID,
// and every allocated SRP-ID must have exactly one intent registered for it.
func TestConcurrentSRPolicyRequestsAllocateUniqueSRPIDs(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("failed to close server connection: %v", err)
		}
	})

	// Drain everything the server writes so sends never block on a full socket buffer.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
		<-done
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)

	const goroutines = 20
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			srPolicy := table.SRPolicy{
				Name:    "concurrent-test",
				SrcAddr: netip.MustParseAddr("10.255.0.1"),
				DstAddr: netip.MustParseAddr("10.255.0.2"),
				Color:   uint32(i),
				Type:    table.PolicyTypeDynamic,
				Metric:  table.TEMetric,
			}
			var err error
			if i%2 == 0 {
				err = ss.SendPCUpdate(srPolicy)
			} else {
				err = ss.RequestSRPolicyCreated(srPolicy)
			}
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Errorf("send returned an error: %v", err)
		}
	}

	ss.srPolicyIntentsMu.Lock()
	gotIntents := len(ss.srPolicyIntents)
	ss.srPolicyIntentsMu.Unlock()
	if gotIntents != goroutines {
		t.Errorf("srPolicyIntents count: got %d, want %d (SRP-IDs must not collide)", gotIntents, goroutines)
	}
	if ss.srpIDHead != uint32(1+goroutines) {
		t.Errorf("srpIDHead: got %d, want %d", ss.srpIDHead, uint32(1+goroutines))
	}
}

func TestAllocateSRPID_SkipsReservedValues(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.srpIDHead = math.MaxUint32 - 1

	for i, want := range []uint32{math.MaxUint32 - 1, 1, 2} {
		got, err := ss.allocateSRPID(table.PolicyTypeDynamic, table.TEMetric, nil)
		if err != nil {
			t.Fatalf("allocation %d: unexpected error: %v", i, err)
		}
		if got == 0 || got == math.MaxUint32 {
			t.Fatalf("allocation %d: got reserved SRP-ID %d", i, got)
		}
		if got != want {
			t.Errorf("allocation %d: got %d, want %d", i, got, want)
		}
	}
}

// The same report with the R-Flag set removes the SR Policy instead of registering it.
func TestHandleStateReportRemove(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	sr := newTestStateReport(t, 1, 0)

	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	sr.LSPObject.RFlag = true
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport returned an error: %v", err)
	}

	if _, found := ss.SearchSRPolicy(sr.LSPObject.PlspID); found {
		t.Error("SR Policy is still registered after being reported as removed")
	}
}

func TestReceiveOpenSeparatesPccAndPolaCapabilities(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("failed to close server connection: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	// The PCC advertises Color Capability support but not LSP Update Capability.
	pccCaps := []pcep.CapabilityInterface{
		&pcep.StatefulPCECapability{
			LSPUpdateCapability: false,
			ColorCapability:     true,
		},
	}
	openMessage, err := pcep.NewOpenMessage(1, 30, pccCaps)
	if err != nil {
		t.Fatalf("failed to create open message: %v", err)
	}
	byteOpenMessage, err := openMessage.Serialize()
	if err != nil {
		t.Fatalf("failed to serialize open message: %v", err)
	}
	if _, err := client.Write(byteOpenMessage); err != nil {
		t.Fatalf("failed to write open message: %v", err)
	}

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	if err := ss.ReceiveOpen(); err != nil {
		t.Fatalf("ReceiveOpen returned an error: %v", err)
	}

	if !reflect.DeepEqual(ss.receivedPccCapabilities, pccCaps) {
		t.Errorf("receivedPccCapabilities: got %+v, want %+v", ss.receivedPccCapabilities, pccCaps)
	}

	wantPolaCaps := pcep.PolaCapability(pccCaps)
	if !reflect.DeepEqual(ss.advertisedCapabilities, wantPolaCaps) {
		t.Errorf("advertisedCapabilities: got %+v, want %+v", ss.advertisedCapabilities, wantPolaCaps)
	}

	sr := newTestStateReport(t, 1, 0)
	sr.LSPObject.TLVs = append(sr.LSPObject.TLVs, &pcep.Color{Color: 100})
	color, _ := ss.resolveColorPreference(sr)
	if color != 100 {
		t.Errorf("resolveColorPreference did not detect Color Capability from receivedPccCapabilities: got color %d, want 100", color)
	}

	receivedCap := ss.receivedPccCapabilities[0].(*pcep.StatefulPCECapability)
	polaCap := ss.advertisedCapabilities[0].(*pcep.StatefulPCECapability)
	if receivedCap == polaCap {
		t.Error("receivedPccCapabilities and advertisedCapabilities share the same StatefulPCECapability instance")
	}
	if receivedCap.LSPUpdateCapability == polaCap.LSPUpdateCapability {
		t.Error("expected received and advertised StatefulPCECapability to diverge, got identical LSPUpdateCapability")
	}
}

// forceFRR must override auto-detection: FRRouting advertises the same
// capabilities as any other RFC-compliant PCC, so it cannot be told apart
// from the OPEN message alone.
func TestReceiveOpenForceFRROverridesDetection(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("failed to close server connection: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	// No capability hints at Cisco or Juniper, so auto-detection alone would
	// classify this peer as RFCCompliant.
	openMessage, err := pcep.NewOpenMessage(1, 30, nil)
	if err != nil {
		t.Fatalf("failed to create open message: %v", err)
	}
	byteOpenMessage, err := openMessage.Serialize()
	if err != nil {
		t.Fatalf("failed to serialize open message: %v", err)
	}
	if _, err := client.Write(byteOpenMessage); err != nil {
		t.Fatalf("failed to write open message: %v", err)
	}

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.forceFRR = true
	if err := ss.ReceiveOpen(); err != nil {
		t.Fatalf("ReceiveOpen returned an error: %v", err)
	}

	if ss.pccType != pcep.FRRoutingLegacy {
		t.Errorf("pccType: got %v, want FRRoutingLegacy", ss.pccType)
	}
}

// forceNokia must override auto-detection the same way forceFRR does: Nokia
// SR OS advertises the same capabilities as any other RFC-compliant PCC, so
// it cannot be told apart from the OPEN message alone.
func TestReceiveOpenForceNokiaOverridesDetection(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("failed to close server connection: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	openMessage, err := pcep.NewOpenMessage(1, 30, nil)
	if err != nil {
		t.Fatalf("failed to create open message: %v", err)
	}
	byteOpenMessage, err := openMessage.Serialize()
	if err != nil {
		t.Fatalf("failed to serialize open message: %v", err)
	}
	if _, err := client.Write(byteOpenMessage); err != nil {
		t.Fatalf("failed to write open message: %v", err)
	}

	ss := NewSession(1, netip.MustParseAddr("213.119.192.12"), server, zap.NewNop(), nil, 0)
	ss.forceNokia = true
	if err := ss.ReceiveOpen(); err != nil {
		t.Fatalf("ReceiveOpen returned an error: %v", err)
	}

	if ss.pccType != pcep.NokiaLegacy {
		t.Errorf("pccType: got %v, want NokiaLegacy", ss.pccType)
	}
}

// SendPCInitiate must only attach the LSP object's Color TLV
// (draft-ietf-pce-pcep-color / RFC 9863) when the peer's OPEN advertised the
// Color Capability bit. Some real PCCs (e.g. Nokia SR OS) do not advertise
// it and are not guaranteed to tolerate an unrecognized TLV there.
func TestSendPCInitiate_ColorTLVGatedOnCapability(t *testing.T) {
	cases := map[string]struct {
		caps    []pcep.CapabilityInterface
		wantTLV bool
	}{
		"ColorCapabilityAdvertised": {
			caps:    []pcep.CapabilityInterface{&pcep.StatefulPCECapability{ColorCapability: true}},
			wantTLV: true,
		},
		"ColorCapabilityNotAdvertised": {
			caps:    []pcep.CapabilityInterface{&pcep.StatefulPCECapability{ColorCapability: false}},
			wantTLV: false,
		},
		"NoCapabilitiesReceived": {
			caps:    nil,
			wantTLV: false,
		},
	}

	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			server, client := newTCPConnPair(t)
			t.Cleanup(func() {
				_ = client.Close()
			})

			ss := NewSession(1, netip.MustParseAddr("213.119.192.12"), server, zap.NewNop(), nil, 0)
			ss.isSynced = true
			ss.receivedPccCapabilities = tt.caps

			srPolicy := table.SRPolicy{
				Name:        "color-gating-test",
				SegmentList: []table.Segment{table.NewSegmentSRMPLS(524044)},
				SrcAddr:     netip.MustParseAddr("213.119.192.12"),
				DstAddr:     netip.MustParseAddr("213.119.192.10"),
				Color:       200,
				Preference:  100,
			}

			if err := ss.SendPCInitiate(srPolicy, false); err != nil {
				t.Fatalf("SendPCInitiate failed: %v", err)
			}

			if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("failed to set read deadline: %v", err)
			}
			header := make([]byte, pcep.CommonHeaderLength)
			if _, err := io.ReadFull(client, header); err != nil {
				t.Fatalf("failed to read PCEP common header: %v", err)
			}
			var ch pcep.CommonHeader
			if err := ch.DecodeFromBytes(header); err != nil {
				t.Fatalf("failed to decode common header: %v", err)
			}
			bodyLen := int(ch.MessageLength) - int(pcep.CommonHeaderLength)
			body := make([]byte, bodyLen)
			if _, err := io.ReadFull(client, body); err != nil {
				t.Fatalf("failed to read PCEP message body: %v", err)
			}

			gotTLV := false
			for i := 0; i+4 <= len(body); i++ {
				if body[i] == 0x00 && body[i+1] == 0x43 && body[i+2] == 0x00 && body[i+3] == 0x04 {
					gotTLV = true
					break
				}
			}
			if gotTLV != tt.wantTLV {
				t.Errorf("Color TLV present on wire: got %v, want %v (body: %x)", gotTLV, tt.wantTLV, body)
			}
		})
	}
}

func TestSweepExpiredSRPolicyIntents_RemovesExpired(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.srPolicyIntentsMu.Lock()
	ss.srPolicyIntents = map[uint32]srPolicyIntent{
		1: {polType: table.PolicyTypeDynamic, metric: table.TEMetric, expiresAt: time.Now().Add(-time.Second)},
	}
	ss.srPolicyIntentsMu.Unlock()

	ss.sweepExpiredSRPolicyIntents()

	if _, ok := ss.takeSRPolicyIntent(1); ok {
		t.Error("expired intent was not removed by the sweeper")
	}
}

func TestSweepExpiredSRPolicyIntents_KeepsUnexpired(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.srPolicyIntentsMu.Lock()
	ss.srPolicyIntents = map[uint32]srPolicyIntent{
		1: {polType: table.PolicyTypeDynamic, metric: table.TEMetric, expiresAt: time.Now().Add(time.Hour)},
	}
	ss.srPolicyIntentsMu.Unlock()

	ss.sweepExpiredSRPolicyIntents()

	if _, ok := ss.takeSRPolicyIntent(1); !ok {
		t.Error("unexpired intent was removed by the sweeper")
	}
}

func TestSweepExpiredSRPolicyIntents_KeepsUnrelatedIntent(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.rememberSRPolicyIntent(1, table.PolicyTypeDynamic, table.TEMetric, nil)
	ss.rememberSRPolicyIntent(2, table.PolicyTypeExplicit, table.UnspecifiedMetric, nil)

	if _, ok := ss.takeSRPolicyIntent(1); !ok {
		t.Fatal("expected intent 1 to be present before consuming it")
	}

	ss.sweepExpiredSRPolicyIntents()

	if _, ok := ss.takeSRPolicyIntent(2); !ok {
		t.Error("sweep must not remove an unrelated intent still within its TTL")
	}
}

func TestIntentSweep_RunsInBackgroundAndStopsCleanly(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.srPolicyIntentTTL = 10 * time.Millisecond
	ss.sweepInterval = 5 * time.Millisecond

	ss.startIntentSweep()
	defer ss.stopIntentSweep()

	ss.rememberSRPolicyIntent(1, table.PolicyTypeDynamic, table.TEMetric, nil)

	deadline := time.Now().Add(2 * time.Second)
	for ss.srPolicyIntentExists(1) {
		if time.Now().After(deadline) {
			t.Fatal("intent was not swept in the background within the deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIntentSweep_StopsCleanly(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.startIntentSweep()

	done := make(chan struct{})
	go func() {
		ss.stopIntentSweep()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopIntentSweep did not return; the sweep goroutine may have leaked")
	}
}

func TestIntentSweep_ConcurrentWithIntentConsumption(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.srPolicyIntentTTL = 5 * time.Millisecond
	ss.sweepInterval = 2 * time.Millisecond
	ss.startIntentSweep()
	defer ss.stopIntentSweep()

	var wg sync.WaitGroup
	for i := uint32(1); i <= 20; i++ {
		wg.Add(1)
		go func(srpID uint32) {
			defer wg.Done()
			ss.rememberSRPolicyIntent(srpID, table.PolicyTypeDynamic, table.TEMetric, nil)
			time.Sleep(time.Millisecond)
			ss.takeSRPolicyIntent(srpID)
		}(i)
	}
	wg.Wait()
}

func TestNextUnusedSRPID_SkipsUsedAcrossWraparound(t *testing.T) {
	used := map[uint32]bool{1: true, 2: true, 4: true}
	got, _, err := nextUnusedSRPID(4, 5, func(id uint32) bool { return used[id] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3 {
		t.Errorf("got %d, want 3 (SRP-IDs 4, 1 and 2 are in use and must be skipped)", got)
	}
}

func TestNextUnusedSRPID_ErrorsWhenExhausted(t *testing.T) {
	used := map[uint32]bool{1: true, 2: true, 3: true, 4: true}
	if _, _, err := nextUnusedSRPID(1, 5, func(id uint32) bool { return used[id] }); err == nil {
		t.Fatal("expected an error when every non-reserved SRP-ID is in use")
	}
}

func TestAllocateSRPID_SkipsInUseIDsOnWraparound(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.srpIDHead = math.MaxUint32 - 1

	// Pre-occupy SRP-ID 1 so the wraparound scan must skip over it.
	ss.rememberSRPolicyIntent(1, table.PolicyTypeExplicit, table.UnspecifiedMetric, nil)

	got, err := ss.allocateSRPID(table.PolicyTypeDynamic, table.TEMetric, nil)
	if err != nil || got != math.MaxUint32-1 {
		t.Fatalf("allocation 1: got (%d, %v), want (%d, nil)", got, err, uint32(math.MaxUint32-1))
	}

	got, err = ss.allocateSRPID(table.PolicyTypeDynamic, table.TEMetric, nil)
	if err != nil {
		t.Fatalf("allocation 2: unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("allocation 2: got %d, want 2 (SRP-ID 1 is still in use and must be skipped)", got)
	}
	if _, ok := ss.takeSRPolicyIntent(2); !ok {
		t.Error("allocateSRPID must register an intent for the SRP-ID it returns")
	}
}

// concurrentSendCase defines a PCEP message send used by the concurrent-send test.
type concurrentSendCase struct {
	name string
	send func(ss *Session, i int) error
}

var concurrentSendCases = []concurrentSendCase{
	{
		name: "SendKeepalive",
		send: func(ss *Session, i int) error { return ss.SendKeepalive() },
	},
	{
		name: "SendOpen",
		send: func(ss *Session, i int) error { return ss.SendOpen() },
	},
	{
		name: "SendClose",
		send: func(ss *Session, i int) error {
			return ss.SendClose(pcep.CloseReasonNoExplanationProvided)
		},
	},
	{
		name: "SendPCUpdate",
		send: func(ss *Session, i int) error {
			return ss.SendPCUpdate(table.SRPolicy{
				Name:    "concurrent-send-test",
				SrcAddr: netip.MustParseAddr("10.255.0.1"),
				DstAddr: netip.MustParseAddr("10.255.0.2"),
				Type:    table.PolicyTypeDynamic,
				Metric:  table.TEMetric,
			})
		},
	},
	{
		name: "RequestSRPolicyCreated",
		send: func(ss *Session, i int) error {
			return ss.RequestSRPolicyCreated(table.SRPolicy{
				Name:    "concurrent-send-test",
				SrcAddr: netip.MustParseAddr("10.255.0.1"),
				DstAddr: netip.MustParseAddr("10.255.0.2"),
				Color:   uint32(i),
				Type:    table.PolicyTypeDynamic,
				Metric:  table.TEMetric,
			})
		},
	},
}

func sendConcurrentPCEPMessages(t *testing.T, ss *Session, goroutines int) {
	t.Helper()

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := concurrentSendCases[i%len(concurrentSendCases)]
			errCh <- c.send(ss, i)
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Errorf("send returned an error: %v", err)
		}
	}
}

// readPCEPMessage reads and validates one PCEP message.
func readPCEPMessage(r io.Reader) error {
	headerBytes := make([]byte, pcep.CommonHeaderLength)
	if _, err := io.ReadFull(r, headerBytes); err != nil {
		return err
	}

	var header pcep.CommonHeader
	if err := header.DecodeFromBytes(headerBytes); err != nil {
		return fmt.Errorf("failed to decode a PCEP common header; sends may have interleaved: %w", err)
	}

	bodyLen := int(header.MessageLength) - int(pcep.CommonHeaderLength)
	if bodyLen == 0 {
		return nil
	}
	_, err := io.ReadFull(r, make([]byte, bodyLen))
	return err
}

// startPCEPFramingValidator validates PCEP message framing in the background.
func startPCEPFramingValidator(r io.Reader, wantCount int) <-chan error {
	result := make(chan error, 1)

	go func() {
		for i := 0; i < wantCount; i++ {
			if err := readPCEPMessage(r); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	}()

	return result
}

func TestSendPCEPMessage_ConcurrentSendsDoNotInterleave(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)

	const goroutines = 40

	readerErr := startPCEPFramingValidator(client, goroutines)

	sendConcurrentPCEPMessages(t, ss, goroutines)

	select {
	case err := <-readerErr:
		if err != nil {
			t.Fatalf("reader failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not observe all messages on the wire; sends may have interleaved and corrupted framing")
	}
}

func TestSendPCEPMessage_UnlocksAfterSendFailure(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)

	if err := server.Close(); err != nil {
		t.Fatalf("failed to close server connection: %v", err)
	}

	if err := ss.SendKeepalive(); err == nil {
		t.Fatal("expected SendKeepalive to fail once the connection is closed")
	}

	done := make(chan error, 1)
	go func() {
		done <- ss.SendKeepalive()
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected second SendKeepalive to fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second SendKeepalive blocked; send mutex may not have been released")
	}
}

func TestFindRouterIDFromAddress(t *testing.T) {
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{}}

	v4Node := table.NewLsNode(0, "router-v4")
	v4Prefix := table.NewLsPrefix(v4Node)
	v4Prefix.Prefix = netip.MustParsePrefix("192.0.2.10/32")
	v4Node.Prefixes = append(v4Node.Prefixes, v4Prefix)
	ted.Nodes[v4Node.RouterID] = v4Node

	v6Node := table.NewLsNode(0, "router-v6")
	v6Prefix := table.NewLsPrefix(v6Node)
	v6Prefix.Prefix = netip.MustParsePrefix("2001:db8::1/128")
	v6Node.Prefixes = append(v6Node.Prefixes, v6Prefix)
	ted.Nodes[v6Node.RouterID] = v6Node

	subnetNode := table.NewLsNode(0, "router-subnet")
	subnetPrefix := table.NewLsPrefix(subnetNode)
	subnetPrefix.Prefix = netip.MustParsePrefix("192.0.2.0/24")
	subnetNode.Prefixes = append(subnetNode.Prefixes, subnetPrefix)
	ted.Nodes[subnetNode.RouterID] = subnetNode

	// No prefixes: only matchable by Router ID.
	idNode := table.NewLsNode(0, "198.51.100.1")
	ted.Nodes[idNode.RouterID] = idNode

	ss := &Session{ted: ted}
	addrIndex := buildAddressRouterIDIndex(ted)

	cases := []struct {
		name    string
		addr    netip.Addr
		want    string
		wantErr bool
	}{
		{"ipv4 prefix", netip.MustParseAddr("192.0.2.10"), "router-v4", false},
		{"ipv6 prefix", netip.MustParseAddr("2001:db8::1"), "router-v6", false},
		{"non-host prefix network address", netip.MustParseAddr("192.0.2.0"), "router-subnet", false},
		{"router id match", netip.MustParseAddr("198.51.100.1"), "198.51.100.1", false},
		{"not found", netip.MustParseAddr("203.0.113.5"), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ss.findRouterIDFromAddress(ted, addrIndex, tc.addr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got routerID %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractSrcDstRouterIDs(t *testing.T) {
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{}}

	srcNode := table.NewLsNode(0, "src-router")
	srcPrefix := table.NewLsPrefix(srcNode)
	srcPrefix.Prefix = netip.MustParsePrefix("10.255.0.1/32")
	srcNode.Prefixes = append(srcNode.Prefixes, srcPrefix)
	ted.Nodes[srcNode.RouterID] = srcNode

	dstNode := table.NewLsNode(0, "10.255.0.2")
	ted.Nodes[dstNode.RouterID] = dstNode

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), ted, 0)

	sr := newTestStateReport(t, 1, 0)
	srcRouterID, dstRouterID, err := ss.extractSrcDstRouterIDs(*sr, ted)
	if err != nil {
		t.Fatalf("extractSrcDstRouterIDs failed: %v", err)
	}
	if srcRouterID != "src-router" {
		t.Errorf("srcRouterID: got %q, want %q", srcRouterID, "src-router")
	}
	if dstRouterID != "10.255.0.2" {
		t.Errorf("dstRouterID: got %q, want %q", dstRouterID, "10.255.0.2")
	}
}

func TestExtractSrcDstRouterIDs_AddressNotFound(t *testing.T) {
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{}}
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), ted, 0)

	sr := newTestStateReport(t, 1, 0)
	if _, _, err := ss.extractSrcDstRouterIDs(*sr, ted); err == nil {
		t.Error("expected an error when neither address is present in the TED")
	}
}

// TestIsSynced_ConcurrentAccess verifies that setSynced and IsSynced are synchronized.
func TestIsSynced_ConcurrentAccess(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			ss.setSynced()
		}
	}()

	for i := 0; i < 100; i++ {
		ss.IsSynced()
	}
	<-done
}

func TestSessionTED_ReturnsLatestSetValue(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)

	tedA := &table.LsTED{Nodes: map[string]*table.LsNode{"a": {RouterID: "a"}}}
	tedB := &table.LsTED{Nodes: map[string]*table.LsNode{"b": {RouterID: "b"}}}

	ss.setTED(tedA)
	if got := ss.TED(); got != tedA {
		t.Fatalf("TED() after first setTED: got %p, want %p", got, tedA)
	}

	ss.setTED(tedB)
	if got := ss.TED(); got != tedB {
		t.Fatalf("TED() after second setTED: got %p, want %p", got, tedB)
	}
}

// TestSessionTED_ConcurrentAccess verifies that setTED and TED are
// synchronized - this is the actual regression test for the bug where
// ss.ted was read/written without any lock once it became live-updatable.
// Run with `go test -race` to catch a regression.
func TestSessionTED_ConcurrentAccess(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	tedA := &table.LsTED{Nodes: map[string]*table.LsNode{}}
	tedB := &table.LsTED{Nodes: map[string]*table.LsNode{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if i%2 == 0 {
				ss.setTED(tedA)
			} else {
				ss.setTED(tedB)
			}
		}
	}()

	for i := 0; i < 100; i++ {
		ss.TED()
	}
	<-done
}

// TestComputePathFromTED_UsesLiveTEDNotConstructionTimeTED is the regression
// test for the Session.ted staleness bug: computePathFromTED must reflect
// whatever setTED last delivered, not the TED snapshot passed to NewSession.
func TestComputePathFromTED_UsesLiveTEDNotConstructionTimeTED(t *testing.T) {
	srCapableNode := func(routerID, prefix string, sidIndex uint32) *table.LsNode {
		return &table.LsNode{
			RouterID:  routerID,
			SrgbBegin: 16000,
			SrgbEnd:   17000,
			Prefixes: []*table.LsPrefix{
				{Prefix: netip.MustParsePrefix(prefix), SidIndex: sidIndex, HasSidIndex: true},
			},
		}
	}

	// Construction-time TED: src and dst exist but have no link between them,
	// so CSPF cannot find a path.
	staleSrc := srCapableNode("src-router", "10.255.0.1/32", 1)
	staleDst := srCapableNode("dst-router", "10.255.0.2/32", 2)
	staleTED := &table.LsTED{Nodes: map[string]*table.LsNode{
		staleSrc.RouterID: staleSrc,
		staleDst.RouterID: staleDst,
	}}

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), staleTED, 0)
	sr := newTestStateReport(t, 1, 0)

	if _, err := ss.computePathFromTED(*sr); err == nil {
		t.Fatal("expected an error against the construction-time TED (no link between src and dst)")
	}

	// Live update: same nodes, now linked. newTestStateReport carries no
	// MetricObjects, so selectMetricType falls back to TEMetric for an
	// RFC-compliant session (NewSession's default pccType) - the link must
	// carry that metric type or CSPF hard-fails on "metric not defined".
	linkedSrc := srCapableNode("src-router", "10.255.0.1/32", 1)
	linkedDst := srCapableNode("dst-router", "10.255.0.2/32", 2)
	linkedSrc.Links = []*table.LsLink{{
		LocalNode: linkedSrc, RemoteNode: linkedDst,
		Metrics: []*table.Metric{table.NewMetric(table.TEMetric, 10)},
	}}
	linkedDst.Links = []*table.LsLink{{
		LocalNode: linkedDst, RemoteNode: linkedSrc,
		Metrics: []*table.Metric{table.NewMetric(table.TEMetric, 10)},
	}}
	updatedTED := &table.LsTED{Nodes: map[string]*table.LsNode{
		linkedSrc.RouterID: linkedSrc,
		linkedDst.RouterID: linkedDst,
	}}
	ss.setTED(updatedTED)

	segments, err := ss.computePathFromTED(*sr)
	if err != nil {
		t.Fatalf("computePathFromTED after setTED failed: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment (dst node SID), got %d: %v", len(segments), segments)
	}
}

// srCapableTEDNode builds an SR-MPLS-capable *table.LsNode: SRGB 16000-17000,
// Node SID 16000+sidIndex, with a /32 loopback prefix at addr.
func srCapableTEDNode(routerID, addr string, sidIndex uint32) *table.LsNode {
	return &table.LsNode{
		RouterID:  routerID,
		SrgbBegin: 16000,
		SrgbEnd:   17000,
		Prefixes: []*table.LsPrefix{
			{Prefix: netip.MustParsePrefix(addr + "/32"), SidIndex: sidIndex, HasSidIndex: true},
		},
	}
}

// linkTEDNodes adds a bidirectional TE-metric adjacency between a and b.
// newTestStateReport-driven policies carry no MetricObjects, so
// selectMetricType falls back to TEMetric for an RFC-compliant session
// (NewSession's default pccType) - reoptimizeDynamicPolicies instead always
// uses the policy's own stored Metric, but the tests below use TEMetric
// throughout for consistency with the intents they remember.
func linkTEDNodes(a, b *table.LsNode, metric uint32) {
	a.Links = append(a.Links, &table.LsLink{LocalNode: a, RemoteNode: b, Metrics: []*table.Metric{table.NewMetric(table.TEMetric, metric)}})
	b.Links = append(b.Links, &table.LsLink{LocalNode: b, RemoteNode: a, Metrics: []*table.Metric{table.NewMetric(table.TEMetric, metric)}})
}

// newTestStateReportForAddrs is like newTestStateReport but lets the caller
// control src/dst addresses and the installed SID list, for tests that need
// more than one policy with distinct endpoints on the same session.
func newTestStateReportForAddrs(t *testing.T, plspID, srpID uint32, srcAddr, dstAddr netip.Addr, sids []uint32) *pcep.StateReport {
	t.Helper()
	sr, err := pcep.NewStateReport()
	if err != nil {
		t.Fatalf("failed to create state report: %v", err)
	}
	sr.SrpObject.SrpID = srpID
	sr.LSPObject.PlspID = plspID
	sr.LSPObject.Name = fmt.Sprintf("policy-%d", plspID)
	sr.LSPObject.SrcAddr = srcAddr
	sr.LSPObject.DstAddr = dstAddr
	sr.LSPObject.OFlag = 0x02
	for _, sid := range sids {
		subobj, err := pcep.NewSREroSubobject(table.NewSegmentSRMPLS(sid))
		if err != nil {
			t.Fatalf("failed to create SR ERO subobject: %v", err)
		}
		sr.EroObject.EroSubobjects = append(sr.EroObject.EroSubobjects, subobj)
	}
	return sr
}

// decodePCUpdSegments reads one PCEP message off conn and returns the
// segment list carried by its ERO object - used to confirm what
// reoptimizeDynamicPolicies actually put on the wire, not just that
// SendPCUpdate returned no error.
func decodePCUpdSegments(t *testing.T, conn *net.TCPConn) []table.Segment {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	header := make([]byte, pcep.CommonHeaderLength)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("failed to read common header: %v", err)
	}
	var commonHeader pcep.CommonHeader
	if err := commonHeader.DecodeFromBytes(header); err != nil {
		t.Fatalf("failed to decode common header: %v", err)
	}
	if commonHeader.MessageType != pcep.MessageTypeUpdate {
		t.Fatalf("MessageType = %v, want Update", commonHeader.MessageType)
	}

	body := make([]byte, commonHeader.MessageLength-pcep.CommonHeaderLength)
	if _, err := io.ReadFull(conn, body); err != nil {
		t.Fatalf("failed to read PCUpd body: %v", err)
	}

	const objHeaderLen = 4 // RFC5440 7.2: every PCEP object header is 4 bytes.
	for offset := 0; offset+objHeaderLen <= len(body); {
		var h pcep.CommonObjectHeader
		if err := h.DecodeFromBytes(body[offset:]); err != nil {
			t.Fatalf("failed to decode object header at offset %d: %v", offset, err)
		}
		end := offset + int(h.ObjectLength)
		if h.ObjectClass == pcep.ObjectClassERO {
			var ero pcep.EroObject
			if err := ero.DecodeFromBytes(h.ObjectType, body[offset+objHeaderLen:end]); err != nil {
				t.Fatalf("failed to decode ERO object: %v", err)
			}
			return ero.ToSegmentList()
		}
		offset = end
	}
	t.Fatal("no ERO object found in PCUpd body")
	return nil
}

// assertNothingSent confirms conn receives no bytes within a short window.
func assertNothingSent(t *testing.T, conn *net.TCPConn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected no bytes to be sent, but read succeeded")
	} else if !os.IsTimeout(err) {
		t.Fatalf("expected a read timeout, got: %v", err)
	}
}

func TestReoptimizeDynamicPolicies_PathChangedSendsPCUpdate(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	sr := newTestStateReport(t, 1, 7) // installs stale segments [16002, 16003]
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 99) // SID 16099, differs from stored [16002, 16003]
	linkTEDNodes(src, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Reoptimized: 1}) {
		t.Fatalf("stats = %+v, want only Reoptimized=1", stats)
	}

	got := decodePCUpdSegments(t, clientConn)
	want := []table.Segment{table.NewSegmentSRMPLS(16099)}
	if !slices.EqualFunc(got, want, table.SegmentsEqual) {
		t.Errorf("PCUpd segments = %v, want %v", got, want)
	}
}

// TestReoptimizeDynamicPolicies_ExclusionAppliedOnReoptimize is the test most
// likely to catch a missed wiring-through of Exclude into the reoptimize
// code path: an excluded node's own route is strictly cheaper, so if
// reoptimizeDynamicPolicies silently dropped the persisted Exclude (e.g. by
// calling cspf.CSPF without it), CSPF would just re-select the cheap,
// currently-installed path and report Unchanged - this would still be a
// passing-looking test run, just testing the wrong thing. Asserting the
// actual segments avoid the excluded node's own SID is what makes this a
// real behavioral check instead of a plumbing-only one.
func TestReoptimizeDynamicPolicies_ExclusionAppliedOnReoptimize(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	sr := newTestStateReport(t, 1, 7) // installs [16002, 16003] (src=10.255.0.1, dst=10.255.0.2)
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, []string{"transit-router"})
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}
	if policy, found := ss.SearchSRPolicy(1); !found || !slices.Equal(policy.Exclude, []string{"transit-router"}) {
		t.Fatalf("registered policy.Exclude = %v (found=%v), want [transit-router] - resolvePolicyIntent/updateOrCreatePolicy didn't carry it through", policy, found)
	}

	// Cheap route (cost 20) via transit-router, which will be excluded, would
	// reproduce exactly the currently-installed [16002, 16003] if exclusion
	// were silently ignored. transit2-router offers the only route once
	// transit-router is genuinely removed from consideration - more
	// expensive (cost 30), proving CSPF didn't just pick it by chance.
	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	transit := srCapableTEDNode("transit-router", "10.255.0.9", 2)    // SID 16002 - excluded
	transit2 := srCapableTEDNode("transit2-router", "10.255.0.8", 10) // SID 16010 - the only viable alternative
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 3)            // SID 16003
	linkTEDNodes(src, transit, 10)
	linkTEDNodes(transit, dst, 10)
	linkTEDNodes(src, transit2, 15)
	linkTEDNodes(transit2, dst, 15)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{
		src.RouterID: src, transit.RouterID: transit, transit2.RouterID: transit2, dst.RouterID: dst,
	}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Reoptimized: 1}) {
		t.Fatalf("stats = %+v, want only Reoptimized=1 (exclusion should force a real path change)", stats)
	}

	got := decodePCUpdSegments(t, clientConn)
	want := []table.Segment{table.NewSegmentSRMPLS(16010), table.NewSegmentSRMPLS(16003)}
	if !slices.EqualFunc(got, want, table.SegmentsEqual) {
		t.Errorf("PCUpd segments = %v, want %v (via transit2-router, avoiding excluded transit-router's SID 16002)", got, want)
	}
}

// TestReoptimizeDynamicPolicies_GlobalExcludeAppliedOnReoptimize mirrors
// TestReoptimizeDynamicPolicies_ExclusionAppliedOnReoptimize, but the
// avoidance comes from ss.globalExcludeStore rather than the policy's own
// Exclude - proving the global node-exclusion set is fetched and applied
// fresh on every reoptimization sweep, exactly like per-policy Exclude.
func TestReoptimizeDynamicPolicies_GlobalExcludeAppliedOnReoptimize(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)
	ss.globalExcludeStore = newGlobalExcludeStore(filepath.Join(t.TempDir(), "global-exclude.json"))
	if err := ss.globalExcludeStore.add("transit-router"); err != nil {
		t.Fatalf("failed to seed global exclude store: %v", err)
	}

	sr := newTestStateReport(t, 1, 7)                                          // installs [16002, 16003] (src=10.255.0.1, dst=10.255.0.2)
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil) // no per-policy exclude
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	transit := srCapableTEDNode("transit-router", "10.255.0.9", 2)    // SID 16002 - globally excluded
	transit2 := srCapableTEDNode("transit2-router", "10.255.0.8", 10) // SID 16010 - the only viable alternative
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 3)            // SID 16003
	linkTEDNodes(src, transit, 10)
	linkTEDNodes(transit, dst, 10)
	linkTEDNodes(src, transit2, 15)
	linkTEDNodes(transit2, dst, 15)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{
		src.RouterID: src, transit.RouterID: transit, transit2.RouterID: transit2, dst.RouterID: dst,
	}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Reoptimized: 1}) {
		t.Fatalf("stats = %+v, want only Reoptimized=1 (global exclusion should force a real path change)", stats)
	}

	got := decodePCUpdSegments(t, clientConn)
	want := []table.Segment{table.NewSegmentSRMPLS(16010), table.NewSegmentSRMPLS(16003)}
	if !slices.EqualFunc(got, want, table.SegmentsEqual) {
		t.Errorf("PCUpd segments = %v, want %v (via transit2-router, avoiding globally excluded transit-router's SID 16002)", got, want)
	}

	if policy, found := ss.SearchSRPolicy(1); !found || len(policy.Exclude) != 0 {
		t.Errorf("policy.Exclude = %v (found=%v), want empty - the global exclusion set must never be baked into a policy's own stored intent", policy.Exclude, found)
	}
}

// TestReoptimizeDynamicPolicies_GlobalExcludeSkippedForOwnEndpoint confirms
// the endpoint-conflict design decision holds during reoptimization too: a
// global exclusion naming this policy's own source must not break its
// reoptimization - it's silently omitted for this policy, so the path stays
// unchanged rather than erroring or going stale.
func TestReoptimizeDynamicPolicies_GlobalExcludeSkippedForOwnEndpoint(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)
	ss.globalExcludeStore = newGlobalExcludeStore(filepath.Join(t.TempDir(), "global-exclude.json"))
	if err := ss.globalExcludeStore.add("src-router"); err != nil { // this policy's own source
		t.Fatalf("failed to seed global exclude store: %v", err)
	}

	sr := newTestStateReport(t, 1, 7) // installs [16002, 16003]
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	transit := srCapableTEDNode("transit-router", "10.255.0.9", 2)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 3)
	linkTEDNodes(src, transit, 10)
	linkTEDNodes(transit, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{
		src.RouterID: src, transit.RouterID: transit, dst.RouterID: dst,
	}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Unchanged: 1}) {
		t.Fatalf("stats = %+v, want only Unchanged=1 (a global exclusion on this policy's own source must be silently skipped, not break reoptimization)", stats)
	}
	assertNothingSent(t, clientConn)
}

func TestReoptimizeDynamicPolicies_PathUnchangedSendsNothing(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	sr := newTestStateReport(t, 1, 7) // installs [16002, 16003]
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	// src -sidIndex 1(unused)-> transit(sidIndex 2 -> SID 16002) -> dst(sidIndex 3 -> SID 16003)
	// so CSPF computes exactly [16002, 16003], matching what's already installed.
	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	transit := srCapableTEDNode("transit-router", "10.255.0.9", 2)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 3)
	linkTEDNodes(src, transit, 10)
	linkTEDNodes(transit, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{
		src.RouterID: src, transit.RouterID: transit, dst.RouterID: dst,
	}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Unchanged: 1}) {
		t.Fatalf("stats = %+v, want only Unchanged=1", stats)
	}
	assertNothingSent(t, clientConn)
}

func TestReoptimizeDynamicPolicies_DestinationUnresolvableIsStale(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	sr := newTestStateReport(t, 1, 7) // dst = 10.255.0.2
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	// TED only knows about the source - the destination dropped out entirely
	// (e.g. the node lost its loopback prefix, or left the TED altogether).
	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Stale: 1}) {
		t.Fatalf("stats = %+v, want only Stale=1", stats)
	}
	assertNothingSent(t, clientConn)
}

func TestReoptimizeDynamicPolicies_NoSRPathIsNoPath(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	sr := newTestStateReport(t, 1, 7)
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	// Both endpoints resolve fine, but there is no link between them at all.
	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 2)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{NoPath: 1}) {
		t.Fatalf("stats = %+v, want only NoPath=1", stats)
	}
	assertNothingSent(t, clientConn)
}

func TestReoptimizeDynamicPolicies_ExplicitTypeNeverTouched(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	sr := newTestStateReport(t, 1, 7) // installs [16002, 16003]
	ss.rememberSRPolicyIntent(7, table.PolicyTypeExplicit, table.UnspecifiedMetric, nil)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	// A TED that would compute a completely different path, if this were dynamic.
	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 99)
	linkTEDNodes(src, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats.total() != 0 {
		t.Fatalf("stats = %+v, want all zero - explicit policies must never be touched", stats)
	}
	assertNothingSent(t, clientConn)
}

func TestReoptimizeDynamicPolicies_UnspecifiedTypeNeverTouched(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	// No TED at construction, so the unsolicited (SRP-ID 0) report registers
	// as-is via handleReportedSRPolicy without ever setting Type/Metric.
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	sr := newTestStateReport(t, 1, 0)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}
	if policy, found := ss.SearchSRPolicy(1); !found || policy.Type != table.PolicyType("") {
		t.Fatalf("test setup invalid: policy.Type = %q, want empty (found=%v)", policy.Type, found)
	}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 99)
	linkTEDNodes(src, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats.total() != 0 {
		t.Fatalf("stats = %+v, want all zero - a policy with unspecified Type must never be touched", stats)
	}
	assertNothingSent(t, clientConn)
}

func TestReoptimizeDynamicPolicies_MultiplePoliciesAggregateStats(t *testing.T) {
	serverConn, clientConn := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	srcAddr := netip.MustParseAddr("10.255.0.1")
	changedDst := netip.MustParseAddr("10.255.0.2")
	staleDst := netip.MustParseAddr("10.255.0.99") // never present in the TED below

	changed := newTestStateReportForAddrs(t, 1, 7, srcAddr, changedDst, []uint32{16002, 16003})
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)
	if err := ss.handleStateReport(changed, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport (changed) failed: %v", err)
	}

	stale := newTestStateReportForAddrs(t, 2, 8, srcAddr, staleDst, []uint32{16002, 16003})
	ss.rememberSRPolicyIntent(8, table.PolicyTypeDynamic, table.TEMetric, nil)
	if err := ss.handleStateReport(stale, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport (stale) failed: %v", err)
	}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 99)
	linkTEDNodes(src, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Reoptimized: 1, Stale: 1}) {
		t.Fatalf("stats = %+v, want Reoptimized=1, Stale=1", stats)
	}

	got := decodePCUpdSegments(t, clientConn)
	want := []table.Segment{table.NewSegmentSRMPLS(16099)}
	if !slices.EqualFunc(got, want, table.SegmentsEqual) {
		t.Errorf("PCUpd segments = %v, want %v", got, want)
	}
}

func TestReoptimizeDynamicPolicies_SendPCUpdateErrorCountsAsErroredNotFatal(t *testing.T) {
	serverConn, _ := newTCPConnPair(t)
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), serverConn, zap.NewNop(), nil, 0)

	for i, srpID := range []uint32{7, 8} {
		sr := newTestStateReport(t, uint32(i+1), srpID)
		ss.rememberSRPolicyIntent(srpID, table.PolicyTypeDynamic, table.TEMetric, nil)
		if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
			t.Fatalf("handleStateReport failed: %v", err)
		}
	}

	src := srCapableTEDNode("src-router", "10.255.0.1", 1)
	dst := srCapableTEDNode("dst-router", "10.255.0.2", 99)
	linkTEDNodes(src, dst, 10)
	ted := &table.LsTED{Nodes: map[string]*table.LsNode{src.RouterID: src, dst.RouterID: dst}}

	// Close the write side so every SendPCUpdate on this session fails.
	if err := serverConn.Close(); err != nil {
		t.Fatalf("failed to close server conn: %v", err)
	}

	stats := ss.reoptimizeDynamicPolicies(ted)
	if stats != (reoptimizeStats{Errored: 2}) {
		t.Fatalf("stats = %+v, want Errored=2 - a send failure on one policy must not stop the others from being attempted", stats)
	}
}

// TestUpdateOrCreatePolicy_CreateBranchFallsBackToIntentStore is the actual
// restart/resync regression test: SRP-ID 0 is exactly what a state-sync
// PCRpt looks like (sent by the PCC on every reconnect, restart or not),
// and never correlates to a remembered ephemeral intent.
func TestUpdateOrCreatePolicy_CreateBranchFallsBackToIntentStore(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))
	if err := ss.intentStore.save(ss.peerAddr, "pe01-policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("failed to seed intent store: %v", err)
	}

	sr := newTestStateReport(t, 1, 0)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	policy, found := ss.SearchSRPolicy(1)
	if !found {
		t.Fatal("policy not registered")
	}
	if policy.Type != table.PolicyTypeDynamic || policy.Metric != table.TEMetric {
		t.Errorf("got Type=%q Metric=%v, want Type=%q Metric=%v", policy.Type, policy.Metric, table.PolicyTypeDynamic, table.TEMetric)
	}
}

func TestUpdateOrCreatePolicy_UpdateBranchFallsBackToIntentStore(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))

	// First registration: no intent anywhere yet, policy created with Type unset.
	sr := newTestStateReport(t, 1, 0)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("first handleStateReport failed: %v", err)
	}
	if policy, _ := ss.SearchSRPolicy(1); policy.Type != table.PolicyType("") {
		t.Fatalf("test setup invalid: expected Type unset after first registration, got %q", policy.Type)
	}

	if err := ss.intentStore.save(ss.peerAddr, "pe01-policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("failed to seed intent store: %v", err)
	}

	// Second report for the same PlspID with a newer LSPID -> update branch.
	sr2 := newTestStateReport(t, 1, 0)
	sr2.LSPObject.LSPID = 2
	if err := ss.handleStateReport(sr2, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("second handleStateReport failed: %v", err)
	}

	policy, found := ss.SearchSRPolicy(1)
	if !found {
		t.Fatal("policy not found")
	}
	if policy.Type != table.PolicyTypeDynamic || policy.Metric != table.TEMetric {
		t.Errorf("got Type=%q Metric=%v, want Type=%q Metric=%v", policy.Type, policy.Metric, table.PolicyTypeDynamic, table.TEMetric)
	}
}

func TestUpdateOrCreatePolicy_SRPIDIntentTakesPrecedenceOverStore(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))
	if err := ss.intentStore.save(ss.peerAddr, "pe01-policy1", table.PolicyTypeExplicit, table.UnspecifiedMetric, nil); err != nil {
		t.Fatalf("failed to seed intent store: %v", err)
	}
	ss.rememberSRPolicyIntent(7, table.PolicyTypeDynamic, table.TEMetric, nil)

	sr := newTestStateReport(t, 1, 7)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}

	policy, found := ss.SearchSRPolicy(1)
	if !found {
		t.Fatal("policy not found")
	}
	if policy.Type != table.PolicyTypeDynamic || policy.Metric != table.TEMetric {
		t.Errorf("SRP-ID intent should take precedence over the store: got Type=%q Metric=%v, want Type=%q Metric=%v",
			policy.Type, policy.Metric, table.PolicyTypeDynamic, table.TEMetric)
	}
}

func TestSendPCUpdate_PersistsIntent(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))

	srPolicy := table.SRPolicy{
		Name: "policy1", SrcAddr: netip.MustParseAddr("10.255.0.1"), DstAddr: netip.MustParseAddr("10.255.0.2"),
		Type: table.PolicyTypeDynamic, Metric: table.TEMetric,
	}
	if err := ss.SendPCUpdate(srPolicy); err != nil {
		t.Fatalf("SendPCUpdate failed: %v", err)
	}

	polType, metric, _, ok := ss.intentStore.lookup(ss.peerAddr, "policy1")
	if !ok {
		t.Fatal("expected intent to be persisted after SendPCUpdate")
	}
	if polType != table.PolicyTypeDynamic || metric != table.TEMetric {
		t.Errorf("got Type=%q Metric=%v, want Type=%q Metric=%v", polType, metric, table.PolicyTypeDynamic, table.TEMetric)
	}
}

func TestSendPCInitiate_PersistsIntent(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))

	srPolicy := table.SRPolicy{
		Name: "policy1", SrcAddr: netip.MustParseAddr("10.255.0.1"), DstAddr: netip.MustParseAddr("10.255.0.2"),
		Type: table.PolicyTypeDynamic, Metric: table.IGPMetric,
		SegmentList: []table.Segment{table.NewSegmentSRMPLS(16001)},
	}
	if err := ss.SendPCInitiate(srPolicy, false); err != nil {
		t.Fatalf("SendPCInitiate failed: %v", err)
	}

	polType, metric, _, ok := ss.intentStore.lookup(ss.peerAddr, "policy1")
	if !ok {
		t.Fatal("expected intent to be persisted after SendPCInitiate")
	}
	if polType != table.PolicyTypeDynamic || metric != table.IGPMetric {
		t.Errorf("got Type=%q Metric=%v, want Type=%q Metric=%v", polType, metric, table.PolicyTypeDynamic, table.IGPMetric)
	}
}

func TestSendPCInitiate_DeleteDoesNotPersistIntent(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))

	srPolicy := table.SRPolicy{Name: "policy1", PlspID: 1, Type: table.PolicyTypeDynamic, Metric: table.TEMetric}
	if err := ss.SendPCInitiate(srPolicy, true); err != nil {
		t.Fatalf("SendPCInitiate (delete) failed: %v", err)
	}

	if _, _, _, ok := ss.intentStore.lookup(ss.peerAddr, "policy1"); ok {
		t.Error("expected no intent to be persisted for a delete request - it's pointless, DeleteSRPolicy's cleanup handles removal")
	}
}

// TestSendPCUpdate_FailedSendDoesNotPersistIntent validates the chosen
// persist timing: after a confirmed successful send, not right after
// allocateSRPID - a request that never left the box shouldn't leave a
// durable "intent" behind.
func TestSendPCUpdate_FailedSendDoesNotPersistIntent(t *testing.T) {
	server, client := newTCPConnPair(t)
	if err := client.Close(); err != nil {
		t.Fatalf("failed to close client connection: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("failed to close server connection: %v", err)
	}

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))

	srPolicy := table.SRPolicy{Name: "policy1", Type: table.PolicyTypeDynamic, Metric: table.TEMetric}
	if err := ss.SendPCUpdate(srPolicy); err == nil {
		t.Fatal("expected SendPCUpdate to fail once the connection is closed")
	}

	if _, _, _, ok := ss.intentStore.lookup(ss.peerAddr, "policy1"); ok {
		t.Error("expected no intent to be persisted for a failed send")
	}
}

// TestSendPCUpdate_RepeatedUnchangedDoesNotRewriteFile ties the intentStore
// hot-path safety fix (save() no-ops on an unchanged value) directly to the
// code path that motivated it: SendPCUpdate fires on every TED-triggered
// reoptimize and every spontaneous PCC re-report, where Type/Metric almost
// never actually change.
func TestSendPCUpdate_RepeatedUnchangedDoesNotRewriteFile(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("failed to close client connection: %v", err)
		}
	})

	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), server, zap.NewNop(), nil, 0)
	path := filepath.Join(t.TempDir(), "intents.json")
	ss.intentStore = newIntentStore(path)

	srPolicy := table.SRPolicy{Name: "policy1", Type: table.PolicyTypeDynamic, Metric: table.TEMetric}
	if err := ss.SendPCUpdate(srPolicy); err != nil {
		t.Fatalf("first SendPCUpdate failed: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}

	if err := ss.SendPCUpdate(srPolicy); err != nil {
		t.Fatalf("second SendPCUpdate failed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no rewrite for an unchanged intent, but the file exists (err=%v)", err)
	}
}

func TestDeleteSRPolicy_RemovesPersistedIntent(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	ss.intentStore = newIntentStore(filepath.Join(t.TempDir(), "intents.json"))

	sr := newTestStateReport(t, 1, 0)
	if err := ss.handleStateReport(sr, pcep.NewPCRptMessage()); err != nil {
		t.Fatalf("handleStateReport failed: %v", err)
	}
	if err := ss.intentStore.save(ss.peerAddr, "pe01-policy1", table.PolicyTypeDynamic, table.TEMetric, nil); err != nil {
		t.Fatalf("failed to seed intent store: %v", err)
	}

	deleteReport := newTestStateReport(t, 1, 0)
	deleteReport.LSPObject.RFlag = true
	ss.DeleteSRPolicy(*deleteReport)

	if _, _, _, ok := ss.intentStore.lookup(ss.peerAddr, "pe01-policy1"); ok {
		t.Error("expected persisted intent to be removed after DeleteSRPolicy")
	}
}

func TestDeleteSRPolicy_UnregisteredPlspIDDoesNotTouchStore(t *testing.T) {
	ss := NewSession(1, netip.MustParseAddr("10.0.255.1"), nil, zap.NewNop(), nil, 0)
	path := filepath.Join(t.TempDir(), "intents.json")
	ss.intentStore = newIntentStore(path)

	deleteReport := newTestStateReport(t, 99, 0) // never registered
	deleteReport.LSPObject.RFlag = true
	ss.DeleteSRPolicy(*deleteReport)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file to be created, since nothing matched (err=%v)", err)
	}
}

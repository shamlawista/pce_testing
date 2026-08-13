// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	pb "github.com/nttcom/pola/api/pola/v1"
	"github.com/nttcom/pola/pkg/packet/pcep"
)

// TestReproPushTestYAML replays, byte-for-byte, the exact CreateSRPolicy
// request built from the operator's push-test.yaml:
//
//	srPolicy:
//	  pcepSessionAddr: 213.119.192.12
//	  srcAddr: 213.119.192.12
//	  dstAddr: 213.119.192.10
//	  name: adjacency-push-test
//	  color: 200
//	  segmentList:
//	    - sid: 524044
//	      localAddr: 213.119.194.9
//	      remoteAddr: 213.119.194.10
//
// run via `pola sr-policy add -f push-test.yaml --no-sid-validate`, and dumps
// the raw PCInitiate message bytes actually written to the wire so they can
// be inspected against RFC 8664 §4.3.1 without needing the live peer.
//
// The session is pinned to pcep.NokiaLegacy, confirmed live against a Nokia
// 7750 (SR OS 26.7.R1): its PCEP parser closes the session with reason 3
// ("malformed PCEP message") when the ASSOCIATION object carries the
// RFC 9862 SRPOLICY-CPATH-ID/PREFERENCE TLVs, so PCInitiate keeps a minimal
// ASSOCIATION object (EXTENDED-ASSOCIATION-ID only) for this pccType -
// asserted below.
//
// Note: localInterfaceId/remoteInterfaceId/unnumbered are all absent, so this
// segment resolves to NAITypeSRIPv4Adjacency (NT=3, "numbered" IPv4
// adjacency) - a path the existing nokia_adjacency_sid_wire_test.go does NOT
// cover (it only exercises NT=5 unnumbered and NT=6 IPv6 link-local).
func TestReproPushTestYAML(t *testing.T) {
	server, client := newTCPConnPair(t)
	t.Cleanup(func() {
		_ = client.Close()
	})

	peerAddr := netip.MustParseAddr("213.119.192.12")
	ss := NewSession(1, peerAddr, server, zap.NewNop(), nil, 0)
	ss.isSynced = true
	ss.pccType = pcep.NokiaLegacy

	pce := &Server{sessionList: []*Session{ss}}
	apiServer := &APIServer{pce: pce, logger: zap.NewNop()}

	req := &pb.CreateSRPolicyRequest{
		SrPolicy: &pb.SRPolicy{
			PcepSessionAddr: peerAddr.AsSlice(),
			SrcAddr:         netip.MustParseAddr("213.119.192.12").AsSlice(),
			DstAddr:         netip.MustParseAddr("213.119.192.10").AsSlice(),
			Color:           200,
			PolicyName:      "adjacency-push-test",
			Type:            pb.SRPolicyType_SR_POLICY_TYPE_EXPLICIT,
			SegmentList: []*pb.Segment{
				{
					Sid:        "524044",
					LocalAddr:  "213.119.194.9",
					RemoteAddr: "213.119.194.10",
				},
			},
		},
		DisablePathCompute: true,
		NoSidValidate:      true,
	}

	resp, err := apiServer.CreateSRPolicy(context.Background(), req)
	require.NoError(t, err, "CreateSRPolicy failed")
	require.True(t, resp.GetIsSuccess())

	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))

	header := make([]byte, pcep.CommonHeaderLength)
	_, err = io.ReadFull(client, header)
	require.NoError(t, err, "failed to read PCEP common header")

	var ch pcep.CommonHeader
	require.NoError(t, ch.DecodeFromBytes(header))
	t.Logf("message type: %s", ch.MessageType.String())

	bodyLen := binary.BigEndian.Uint16(header[2:4]) - uint16(pcep.CommonHeaderLength)
	body := make([]byte, bodyLen)
	_, err = io.ReadFull(client, body)
	require.NoError(t, err, "failed to read PCEP message body")

	full := append(header, body...)
	t.Logf("declared MessageLength: %d, actual bytes on wire: %d", ch.MessageLength, len(full))
	t.Logf("full PCInitiate message (hex): %s", hex.EncodeToString(full))
	dumpObjects(t, body)

	const associationObjectClass = 40
	assocBody := findObjectBody(t, body, associationObjectClass)
	tlvTypes := tlvTypesIn(t, assocBody[12:]) // skip the fixed reserved/flags/assoctype/associd/assocsrc(IPv4) header

	const extendedAssociationIDType = 0x1f   // 31, RFC 8697
	const srPolicyCPathIDType = 0x39         // 57, RFC 9862
	const srPolicyCPathPreferenceType = 0x3b // 59, RFC 9862

	assert.Contains(t, tlvTypes, uint16(extendedAssociationIDType), "NokiaLegacy ASSOCIATION object should keep EXTENDED-ASSOCIATION-ID for color/endpoint")
	assert.NotContains(t, tlvTypes, uint16(srPolicyCPathIDType), "NokiaLegacy ASSOCIATION object must not carry SRPOLICY-CPATH-ID (RFC 9862)")
	assert.NotContains(t, tlvTypes, uint16(srPolicyCPathPreferenceType), "NokiaLegacy ASSOCIATION object must not carry SRPOLICY-CPATH-PREFERENCE (RFC 9862)")

	// Confirmed live: this Nokia box closes the session ("ObjClass 40 ObjType 1
	// out of order") when ASSOCIATION follows ERO, so it must come first.
	classes := objectClassSequence(t, body)
	assocIdx := indexOf(classes, associationObjectClass)
	eroIdx := indexOf(classes, eroObjectClass)
	require.GreaterOrEqual(t, assocIdx, 0, "ASSOCIATION object not found")
	require.GreaterOrEqual(t, eroIdx, 0, "ERO object not found")
	assert.Less(t, assocIdx, eroIdx, "ASSOCIATION must be serialized before ERO for a NokiaLegacy peer")
}

// objectClassSequence returns the PCEP object class byte of every object in
// body, in wire order.
func objectClassSequence(t *testing.T, body []byte) []byte {
	t.Helper()
	var classes []byte
	off := 0
	for off+4 <= len(body) {
		objLen := int(binary.BigEndian.Uint16(body[off+2 : off+4]))
		if objLen < 4 || off+objLen > len(body) {
			break
		}
		classes = append(classes, body[off])
		off += objLen
	}
	return classes
}

func indexOf(classes []byte, want byte) int {
	for i, c := range classes {
		if c == want {
			return i
		}
	}
	return -1
}

// tlvTypesIn walks a TLV-only byte region (type(2)+length(2)+value+padding)
// and returns the type of each TLV found.
func tlvTypesIn(t *testing.T, tlvBytes []byte) []uint16 {
	t.Helper()
	var types []uint16
	off := 0
	for off+4 <= len(tlvBytes) {
		typ := binary.BigEndian.Uint16(tlvBytes[off : off+2])
		valLen := int(binary.BigEndian.Uint16(tlvBytes[off+2 : off+4]))
		types = append(types, typ)
		padded := (valLen + 3) &^ 3 // round up to a 4-byte boundary
		off += 4 + padded
	}
	return types
}

// dumpObjects walks the PCEP common-object headers in body and logs each
// object's class/type/length plus its raw payload, to make framing bugs
// (e.g. a length field not matching what was actually written) visible.
func dumpObjects(t *testing.T, body []byte) {
	t.Helper()
	off := 0
	for off < len(body) {
		if len(body)-off < 4 {
			t.Logf("trailing %d byte(s) too short for an object header: %s", len(body)-off, hex.EncodeToString(body[off:]))
			return
		}
		objClass := body[off]
		objType := body[off+1] >> 4
		objLen := binary.BigEndian.Uint16(body[off+2 : off+4])
		if objLen < 4 || int(objLen) > len(body)-off {
			t.Logf("object at offset %d: class=%d type=%d declared length=%d is invalid for remaining body of %d bytes: %s",
				off, objClass, objType, objLen, len(body)-off, hex.EncodeToString(body[off:]))
			return
		}
		t.Logf("object at offset %d: class=%d type=%d length=%d payload=%s",
			off, objClass, objType, objLen, hex.EncodeToString(body[off:off+int(objLen)]))
		off += int(objLen)
	}
}

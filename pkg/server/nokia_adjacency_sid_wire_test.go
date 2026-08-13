// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package server

import (
	"bytes"
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

// TestCreateSRPolicy_PushesAdjacencySIDWireBytes verifies the full
// encode/push direction end-to-end through the real gRPC entry point used by
// `pola sr-policy add`: CreateSRPolicy -> buildSegmentList -> sendSRPolicyRequest
// -> Session.RequestSRPolicyCreated -> SendPCInitiate, writing to a real
// net.TCPConn standing in for the PCEP session to a Nokia 7750 PCC. It
// verifies the raw bytes actually written to that socket contain a
// correctly-formed RFC 8664 §4.3.1 SR-ERO subobject for the requested NAI
// type, not just that the Go-level construction succeeds.
func TestCreateSRPolicy_PushesAdjacencySIDWireBytes(t *testing.T) {
	cases := []struct {
		name       string
		segment    *pb.Segment
		wantNAI    uint8 // NAI type nibble expected in the ERO subobject
		wantNAIHex string
	}{
		{
			name: "UnnumberedAdjacency",
			segment: &pb.Segment{
				Sid: "24002", LocalAddr: "10.255.0.1", RemoteAddr: "10.255.0.2",
				LocalInterfaceId: 5, RemoteInterfaceId: 7, Unnumbered: true,
			},
			wantNAI: 5,
			// Local Node-ID(4) + Local IfID(4) + Remote Node-ID(4) + Remote IfID(4)
			wantNAIHex: "0aff000100000005" + "0aff000200000007",
		},
		{
			name: "IPv6LinkLocalAdjacency",
			segment: &pb.Segment{
				Sid: "24003", LocalAddr: "fe80::1", RemoteAddr: "fe80::2",
				LocalInterfaceId: 3, RemoteInterfaceId: 9,
			},
			wantNAI: 6,
			// Local IPv6(16) + Local IfID(4) + Remote IPv6(16) + Remote IfID(4)
			wantNAIHex: "fe800000000000000000000000000001" + "00000003" +
				"fe800000000000000000000000000002" + "00000009",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, client := newTCPConnPair(t)
			t.Cleanup(func() {
				_ = client.Close()
			})

			peerAddr := netip.MustParseAddr("192.0.2.1") // stand-in for the Nokia 7750 PCC
			ss := NewSession(1, peerAddr, server, zap.NewNop(), nil, 0)
			ss.isSynced = true

			pce := &Server{sessionList: []*Session{ss}}
			apiServer := &APIServer{pce: pce, logger: zap.NewNop()}

			req := &pb.CreateSRPolicyRequest{
				SrPolicy: &pb.SRPolicy{
					PcepSessionAddr: peerAddr.AsSlice(),
					SrcAddr:         netip.MustParseAddr("10.255.0.1").AsSlice(),
					DstAddr:         netip.MustParseAddr("10.255.0.2").AsSlice(),
					Color:           100,
					PolicyName:      "nokia-adjacency-sid-test",
					Type:            pb.SRPolicyType_SR_POLICY_TYPE_EXPLICIT,
					SegmentList:     []*pb.Segment{tc.segment},
				},
				DisablePathCompute: true,
				NoSidValidate:      true,
			}

			resp, err := apiServer.CreateSRPolicy(context.Background(), req)
			require.NoError(t, err, "CreateSRPolicy failed")
			assert.True(t, resp.GetIsSuccess())

			require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))

			header := make([]byte, pcep.CommonHeaderLength)
			_, err = io.ReadFull(client, header)
			require.NoError(t, err, "failed to read PCEP common header")

			var ch pcep.CommonHeader
			require.NoError(t, ch.DecodeFromBytes(header))
			assert.Equal(t, pcep.MessageTypeLSPInitReq, ch.MessageType, "expected a PCInitiate message")

			bodyLen := binary.BigEndian.Uint16(header[2:4]) - uint16(pcep.CommonHeaderLength)
			body := make([]byte, bodyLen)
			_, err = io.ReadFull(client, body)
			require.NoError(t, err, "failed to read PCEP message body")

			// Locate the SR-ERO subobject (Type 0x24) within the message body and
			// confirm its NAI-type nibble and NAI bytes match RFC 8664 §4.3.1 exactly.
			idx := bytes.IndexByte(body, 0x24)
			require.GreaterOrEqualf(t, idx, 0, "SR-ERO subobject (type 0x24) not found in message body: %x", body)
			require.GreaterOrEqual(t, len(body), idx+3, "message body truncated before NAI-type byte")

			gotNAI := body[idx+2] >> 4
			assert.Equalf(t, tc.wantNAI, gotNAI, "NAI type mismatch in wire bytes: %x", body)

			assert.Containsf(t, hex.EncodeToString(body), tc.wantNAIHex, "expected NAI bytes not found in wire message: %x", body)
		})
	}
}

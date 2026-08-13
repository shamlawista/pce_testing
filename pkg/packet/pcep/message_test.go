// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package pcep

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/nttcom/pola/pkg/table"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPCErrMessage_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := map[string]*PCErrMessage{
		"SingleError": {
			Errors: []*PCEPErrorObject{
				{ObjectType: ObjectTypeErrorError, ErrorType: 6, ErrorValue: 1},
			},
		},
		"MultipleErrors": {
			Errors: []*PCEPErrorObject{
				{ObjectType: ObjectTypeErrorError, ErrorType: 6, ErrorValue: 1},
				{ObjectType: ObjectTypeErrorError, ErrorType: 19, ErrorValue: 1},
			},
		},
		"SingleSRPWithError": {
			SRPs: []*SrpObject{
				{ObjectType: ObjectTypeSRPSRP, SrpID: 42},
			},
			Errors: []*PCEPErrorObject{
				{ObjectType: ObjectTypeErrorError, ErrorType: 24, ErrorValue: 1},
			},
		},
		"MultipleSRPsAndErrors": {
			SRPs: []*SrpObject{
				{ObjectType: ObjectTypeSRPSRP, SrpID: 1},
				{ObjectType: ObjectTypeSRPSRP, RFlag: true, SrpID: 2},
			},
			Errors: []*PCEPErrorObject{
				{ObjectType: ObjectTypeErrorError, ErrorType: 6, ErrorValue: 8},
				{ObjectType: ObjectTypeErrorError, ErrorType: 6, ErrorValue: 9},
			},
		},
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, err := want.Serialize()
			require.NoError(t, err, "Serialize failed")

			var commonHeader CommonHeader
			require.NoError(t, commonHeader.DecodeFromBytes(raw[:CommonHeaderLength]), "common header decode failed")
			require.Equal(t, MessageTypeError, commonHeader.MessageType, "unexpected message type")
			require.Equal(t, len(raw), int(commonHeader.MessageLength), "MessageLength must match serialized size")

			var got PCErrMessage
			require.NoError(t, got.DecodeFromBytes(raw[CommonHeaderLength:]), "DecodeFromBytes failed")
			assert.Equal(t, want, &got, "round-trip value mismatch")

			raw2, err := got.Serialize()
			require.NoError(t, err, "re-Serialize failed")
			assert.Equal(t, raw, raw2, "re-serialized bytes differ")
		})
	}
}

// Verify OriginatorASN is propagated to the SRPOLICY-CPATH-ID TLV.
func TestNewPCInitiateMessage_OriginatorASNReachesWire(t *testing.T) {
	t.Parallel()

	srcAddr := netip.MustParseAddr("192.0.2.1")
	dstAddr := netip.MustParseAddr("192.0.2.2")
	segmentList := []table.Segment{table.NewSegmentSRMPLS(16001)}

	cases := map[string]struct {
		opts        []Opt
		expectedASN uint32
	}{
		"OriginatorASNSet":     {opts: []Opt{VendorSpecific(RFCCompliant), OriginatorASN(65000)}, expectedASN: 65000},
		"OriginatorASNOmitted": {opts: []Opt{VendorSpecific(RFCCompliant)}, expectedASN: 0},
	}

	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := NewPCInitiateMessage(1, "policy1", false, 0, segmentList, 100, 200, srcAddr, dstAddr, tt.opts...)
			require.NoError(t, err, "NewPCInitiateMessage failed")
			require.NotNil(t, m.AssociationObject, "RFC compliant PCInitiate must carry an ASSOCIATION object")

			var cpathID *SRPolicyCandidatePathIdentifier
			for _, tlv := range m.AssociationObject.TLVs {
				if id, ok := tlv.(*SRPolicyCandidatePathIdentifier); ok {
					cpathID = id
					break
				}
			}
			require.NotNil(t, cpathID, "SRPOLICY-CPATH-ID TLV missing from ASSOCIATION object")
			assert.Equal(t, tt.expectedASN, cpathID.OriginatorASN, "OriginatorASN not propagated to the TLV")

			raw, err := m.Serialize()
			require.NoError(t, err, "Serialize failed")

			expectedTLV := AppendByteSlices(
				[]uint8{0x00, 0x39, 0x00, 0x1c}, // SRPOLICY-CPATH-ID TLV: type=0x0039, len=28
				[]uint8{0x0a, 0x00, 0x00, 0x00}, // protocol origin + mbz
				Uint32ToByteSlice(tt.expectedASN),
				make([]uint8, 12), dstAddr.AsSlice(), // originator address (IPv4 in the 16 byte field)
				[]uint8{0x00, 0x00, 0x00, 0x01}, // discriminator
			)
			assert.True(t, bytes.Contains(raw, expectedTLV), "serialized message does not carry ASN %d in the SRPOLICY-CPATH-ID TLV", tt.expectedASN)
		})
	}
}

// IncludeColorTLV(false) must omit the LSP object's Color TLV (draft-ietf-pce-pcep-color
// / RFC 9863) entirely; peers that never advertised the Color Capability bit
// may not tolerate an unrecognized TLV there.
func TestNewPCInitiateMessage_ColorTLVGating(t *testing.T) {
	t.Parallel()

	srcAddr := netip.MustParseAddr("192.0.2.1")
	dstAddr := netip.MustParseAddr("192.0.2.2")
	segmentList := []table.Segment{table.NewSegmentSRMPLS(16001)}

	cases := map[string]struct {
		opts    []Opt
		wantTLV bool
	}{
		"CapabilityAdvertised":    {opts: []Opt{VendorSpecific(RFCCompliant), IncludeColorTLV(true)}, wantTLV: true},
		"CapabilityNotAdvertised": {opts: []Opt{VendorSpecific(RFCCompliant), IncludeColorTLV(false)}, wantTLV: false},
		"DefaultOmitted":          {opts: []Opt{VendorSpecific(RFCCompliant)}, wantTLV: true},
	}

	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := NewPCInitiateMessage(1, "policy1", false, 0, segmentList, 100, 200, srcAddr, dstAddr, tt.opts...)
			require.NoError(t, err, "NewPCInitiateMessage failed")

			var gotColor *Color
			for _, tlv := range m.LSPObject.TLVs {
				if c, ok := tlv.(*Color); ok {
					gotColor = c
					break
				}
			}
			if tt.wantTLV {
				require.NotNil(t, gotColor, "expected Color TLV in LSP object")
				assert.Equal(t, uint32(100), gotColor.Color)
			} else {
				assert.Nil(t, gotColor, "Color TLV should be omitted from the LSP object")
			}

			raw, err := m.Serialize()
			require.NoError(t, err, "Serialize failed")
			assert.Equal(t, tt.wantTLV, bytes.Contains(raw, []byte{0x00, 0x43, 0x00, 0x04}), "Color TLV presence on the wire does not match expectation")
		})
	}
}

// Verify object selection for each PccType.
func TestNewPCInitiateMessage_VendorObjectSelection(t *testing.T) {
	t.Parallel()

	srcAddr := netip.MustParseAddr("192.0.2.1")
	dstAddr := netip.MustParseAddr("192.0.2.2")
	segmentList := []table.Segment{table.NewSegmentSRMPLS(16001)}

	cases := map[string]struct {
		pccType         PccType
		wantAssociation bool
		wantAssocType   AssocType
		wantVendorInfo  bool
	}{
		"JuniperLegacy": {
			pccType:         JuniperLegacy,
			wantAssociation: true,
			wantAssocType:   AssociationTypeSRPolicyAssociationJuniper,
		},
		"CiscoLegacy": {
			pccType:        CiscoLegacy,
			wantVendorInfo: true,
		},
		"RFCCompliant": {
			pccType:         RFCCompliant,
			wantAssociation: true,
			wantAssocType:   AssociationTypeSRPolicyAssociation,
			wantVendorInfo:  false,
		},
		"FRRoutingLegacy": {
			pccType:         FRRoutingLegacy,
			wantAssociation: true,
			wantAssocType:   AssociationTypeSRPolicyAssociation,
			wantVendorInfo:  true,
		},
		"NokiaLegacy": {
			pccType:         NokiaLegacy,
			wantAssociation: true,
			wantAssocType:   AssociationTypeSRPolicyAssociation,
			wantVendorInfo:  false,
		},
	}

	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := NewPCInitiateMessage(1, "policy1", false, 0, segmentList, 100, 200, srcAddr, dstAddr, VendorSpecific(tt.pccType))
			require.NoError(t, err, "NewPCInitiateMessage failed")

			if tt.wantAssociation {
				require.NotNil(t, m.AssociationObject, "AssociationObject should be set")
				assert.Equal(t, tt.wantAssocType, m.AssociationObject.AssocType, "unexpected AssocType")
			} else {
				assert.Nil(t, m.AssociationObject, "AssociationObject should not be set")
			}

			if tt.wantVendorInfo {
				require.NotNil(t, m.VendorInformationObject, "VendorInformationObject should be set")
				assert.Equal(t, EnterpriseNumberCisco, m.VendorInformationObject.EnterpriseNumber, "unexpected EnterpriseNumber")
			} else {
				assert.Nil(t, m.VendorInformationObject, "VendorInformationObject should not be set")
			}

			if tt.pccType == NokiaLegacy {
				require.NotNil(t, m.AssociationObject)
				var hasExtendedAssocID, hasCpathID, hasCpathPreference bool
				for _, tlv := range m.AssociationObject.TLVs {
					switch tlv.(type) {
					case *ExtendedAssociationID:
						hasExtendedAssocID = true
					case *SRPolicyCandidatePathIdentifier:
						hasCpathID = true
					case *SRPolicyCandidatePathPreference:
						hasCpathPreference = true
					}
				}
				assert.True(t, hasExtendedAssocID, "NokiaLegacy ASSOCIATION object should keep EXTENDED-ASSOCIATION-ID for color/endpoint")
				assert.False(t, hasCpathID, "NokiaLegacy ASSOCIATION object must not carry SRPOLICY-CPATH-ID (RFC 9862)")
				assert.False(t, hasCpathPreference, "NokiaLegacy ASSOCIATION object must not carry SRPOLICY-CPATH-PREFERENCE (RFC 9862)")
			}
		})
	}
}

// Nokia's PCEP parser closes the session ("ObjClass 40 ObjType 1 out of
// order") when ASSOCIATION follows ERO - the RFC 8697/9862 ABNF-compliant
// position - so NokiaLegacy must serialize it right after LSP instead.
// Other pccTypes keep the spec-compliant after-ERO position.
func TestNewPCInitiateMessage_AssociationObjectOrder(t *testing.T) {
	t.Parallel()

	srcAddr := netip.MustParseAddr("192.0.2.1")
	dstAddr := netip.MustParseAddr("192.0.2.2")
	segmentList := []table.Segment{table.NewSegmentSRMPLS(16001)}

	cases := map[string]struct {
		pccType            PccType
		wantAssocBeforeEro bool
	}{
		"RFCCompliant":    {pccType: RFCCompliant, wantAssocBeforeEro: false},
		"FRRoutingLegacy": {pccType: FRRoutingLegacy, wantAssocBeforeEro: false},
		"NokiaLegacy":     {pccType: NokiaLegacy, wantAssocBeforeEro: true},
	}

	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m, err := NewPCInitiateMessage(1, "policy1", false, 0, segmentList, 100, 200, srcAddr, dstAddr, VendorSpecific(tt.pccType))
			require.NoError(t, err, "NewPCInitiateMessage failed")

			raw, err := m.Serialize()
			require.NoError(t, err, "Serialize failed")

			classes := objectClassSequence(t, raw[CommonHeaderLength:])
			assocIdx := indexOfClass(classes, ObjectClassAssociation)
			eroIdx := indexOfClass(classes, ObjectClassERO)
			require.GreaterOrEqual(t, assocIdx, 0, "ASSOCIATION object not found in wire bytes: classes=%v", classes)
			require.GreaterOrEqual(t, eroIdx, 0, "ERO object not found in wire bytes: classes=%v", classes)

			if tt.wantAssocBeforeEro {
				assert.Less(t, assocIdx, eroIdx, "expected ASSOCIATION before ERO")
			} else {
				assert.Greater(t, assocIdx, eroIdx, "expected ASSOCIATION after ERO")
			}
		})
	}
}

// objectClassSequence walks the common object headers in body (RFC 5440 §7.2)
// and returns the ObjectClass of each object in wire order.
func objectClassSequence(t *testing.T, body []byte) []ObjectClass {
	t.Helper()
	var classes []ObjectClass
	off := 0
	for off+int(commonObjectHeaderLength) <= len(body) {
		var h CommonObjectHeader
		require.NoError(t, h.DecodeFromBytes(body[off:]))
		require.GreaterOrEqualf(t, int(h.ObjectLength), int(commonObjectHeaderLength), "object at offset %d has invalid length %d", off, h.ObjectLength)
		classes = append(classes, h.ObjectClass)
		off += int(h.ObjectLength)
	}
	return classes
}

func indexOfClass(classes []ObjectClass, want ObjectClass) int {
	for i, c := range classes {
		if c == want {
			return i
		}
	}
	return -1
}

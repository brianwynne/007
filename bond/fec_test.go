package bond

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFECEncoderDecoder_Roundtrip(t *testing.T) {
	enc, err := NewFECEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder()

	// Send K=16 packets through encoder, collect encoded data + parity
	var encoded [][]byte
	var nonces []uint64
	for i := 0; i < 16; i++ {
		pkt := []byte{byte(i), 0xAA, 0xBB, 0xCC}
		nonce := uint64(100 + i)
		nonces = append(nonces, nonce)
		data, parity := enc.Encode(pkt, nonce)
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Should have 16 data + 2 parity = 18 packets
	if len(encoded) != 18 {
		t.Fatalf("expected 18 encoded packets, got %d", len(encoded))
	}

	// Decode all data packets — should get payloads back
	for i := 0; i < 16; i++ {
		data, recovered := dec.Decode(encoded[i])
		if data == nil {
			t.Fatalf("packet %d: expected data, got nil", i)
		}
		if data.Nonce != nonces[i] {
			t.Errorf("packet %d: nonce=%d, want %d", i, data.Nonce, nonces[i])
		}
		expected := []byte{byte(i), 0xAA, 0xBB, 0xCC}
		if !bytes.Equal(data.Data, expected) {
			t.Errorf("packet %d: data=%v, want %v", i, data.Data, expected)
		}
		_ = recovered // may trigger cleanup
	}
}

func TestFECRecovery_SingleLoss(t *testing.T) {
	enc, err := NewFECEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder()

	// Encode 16 packets
	var encoded [][]byte
	for i := 0; i < 16; i++ {
		pkt := make([]byte, 100)
		pkt[0] = byte(i)
		binary.BigEndian.PutUint32(pkt[4:8], uint32(i))
		data, parity := enc.Encode(pkt, uint64(i))
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Drop packet 5, feed rest to decoder
	lostIdx := 5
	var recovered []*DecodedPacket
	for i, pkt := range encoded {
		if i == lostIdx {
			continue // simulate loss
		}
		data, rec := dec.Decode(pkt)
		_ = data
		recovered = append(recovered, rec...)
	}

	// Should have recovered exactly 1 packet
	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered packet, got %d", len(recovered))
	}

	// Verify recovered data
	if recovered[0].Nonce != uint64(lostIdx) {
		t.Errorf("recovered nonce=%d, want %d", recovered[0].Nonce, lostIdx)
	}
	if recovered[0].Data[0] != byte(lostIdx) {
		t.Errorf("recovered data[0]=%d, want %d", recovered[0].Data[0], lostIdx)
	}
}

func TestFECRecovery_TwoLoss(t *testing.T) {
	enc, err := NewFECEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder()

	// Default K=16, M=2 — can recover up to 2 lost packets
	var encoded [][]byte
	for i := 0; i < 16; i++ {
		pkt := make([]byte, 50)
		pkt[0] = byte(i)
		data, parity := enc.Encode(pkt, uint64(i))
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Drop packets 3 and 10
	drop := map[int]bool{3: true, 10: true}
	var recovered []*DecodedPacket
	for i, pkt := range encoded {
		if drop[i] {
			continue
		}
		_, rec := dec.Decode(pkt)
		recovered = append(recovered, rec...)
	}

	if len(recovered) != 2 {
		t.Fatalf("expected 2 recovered packets, got %d", len(recovered))
	}

	// Verify nonces
	nonces := map[uint64]bool{}
	for _, r := range recovered {
		nonces[r.Nonce] = true
	}
	if !nonces[3] || !nonces[10] {
		t.Errorf("expected nonces 3 and 10, got %v", nonces)
	}
}

func TestFECRecovery_TooManyLost(t *testing.T) {
	enc, err := NewFECEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder()

	// K=16, M=2 — 3 lost packets is unrecoverable
	var encoded [][]byte
	for i := 0; i < 16; i++ {
		pkt := []byte{byte(i)}
		data, parity := enc.Encode(pkt, uint64(i))
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Drop 3 packets
	drop := map[int]bool{1: true, 7: true, 12: true}
	var recovered []*DecodedPacket
	for i, pkt := range encoded {
		if drop[i] {
			continue
		}
		_, rec := dec.Decode(pkt)
		recovered = append(recovered, rec...)
	}

	if len(recovered) != 0 {
		t.Errorf("expected 0 recovered packets with 3 lost, got %d", len(recovered))
	}
}

func TestFECKeepaliveBypass(t *testing.T) {
	// Empty packets (keepalives) should NOT be FEC-encoded
	// This is handled at the device level, but verify encoder handles empty data
	enc, err := NewFECEncoder()
	if err != nil {
		t.Fatal(err)
	}

	data, parity := enc.Encode([]byte{}, 0)
	// Should still produce encoded data (FEC header + nonce + empty payload)
	if len(data) != FECPayloadOffset {
		t.Errorf("encoded empty packet length=%d, want %d", len(data), FECPayloadOffset)
	}
	if parity != nil {
		t.Error("unexpected parity from single packet")
	}
}

func TestFECHeaderFormat(t *testing.T) {
	enc, err := NewFECEncoder()
	if err != nil {
		t.Fatal(err)
	}

	pkt := []byte{0x45, 0x00, 0x00, 0x1c} // IPv4-like
	data, _ := enc.Encode(pkt, 12345)

	// Check FEC header
	blockID := binary.BigEndian.Uint16(data[0:2])
	index := data[2]
	k := data[3]
	m := data[4]
	nonce := binary.BigEndian.Uint64(data[FECHeaderSize:FECPayloadOffset])

	if blockID != 0 {
		t.Errorf("blockID=%d, want 0", blockID)
	}
	if index != 0 {
		t.Errorf("index=%d, want 0", index)
	}
	if k != 16 {
		t.Errorf("K=%d, want 16", k)
	}
	if m != 2 {
		t.Errorf("M=%d, want 2", m)
	}
	if nonce != 12345 {
		t.Errorf("nonce=%d, want 12345", nonce)
	}

	// Payload should match original
	if !bytes.Equal(data[FECPayloadOffset:], pkt) {
		t.Errorf("payload mismatch")
	}
}

func TestFECControlPacketNotDecoded(t *testing.T) {
	dec := NewFECDecoder()

	// Control packet (blockID = 0xFFFF) should not be decoded as FEC
	pkt := make([]byte, 20)
	binary.BigEndian.PutUint16(pkt[0:2], controlBlockID)
	pkt[2] = controlTypeNACK

	data, recovered := dec.Decode(pkt)
	// k=0, m=0 → should return nil
	if data != nil || recovered != nil {
		t.Error("control packet should not produce decoded data")
	}
}

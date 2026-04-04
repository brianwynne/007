package bond

import (
	"testing"
)

func TestRetransmitBuffer_StoreAndLookup(t *testing.T) {
	rb := &retransmitBuffer{}

	// Store packets
	for i := uint64(0); i < 10; i++ {
		rb.Store([]byte{byte(i), 0xFF}, i)
	}

	// Lookup existing
	data := rb.Lookup(5)
	if data == nil {
		t.Fatal("expected to find nonce 5")
	}
	if data[0] != 5 || data[1] != 0xFF {
		t.Errorf("data=%v, want [5, 255]", data)
	}

	// Lookup returns a copy — modifying shouldn't affect buffer
	data[0] = 99
	data2 := rb.Lookup(5)
	if data2[0] != 5 {
		t.Error("lookup should return independent copies")
	}

	// Lookup non-existent
	data = rb.Lookup(999)
	if data != nil {
		t.Error("expected nil for non-existent nonce")
	}
}

func TestRetransmitBuffer_Wraparound(t *testing.T) {
	rb := &retransmitBuffer{}

	// Fill beyond capacity
	for i := uint64(0); i < retransmitBufSize+100; i++ {
		rb.Store([]byte{byte(i & 0xFF)}, i)
	}

	// Old entries should be overwritten
	data := rb.Lookup(0)
	if data != nil {
		t.Error("nonce 0 should have been overwritten")
	}

	// Recent entries should exist
	data = rb.Lookup(retransmitBufSize + 50)
	if data == nil {
		t.Fatal("expected to find recent nonce")
	}
}

func TestNACKPacket_BuildAndParse(t *testing.T) {
	nonces := []uint64{100, 200, 300, 12345678}
	pkt := buildNACKPacket(nonces)

	// Verify it's a control packet
	if !isControlPacket(pkt) {
		t.Error("NACK should be a control packet")
	}

	// Parse it back
	parsed := parseNACKPacket(pkt)
	if len(parsed) != len(nonces) {
		t.Fatalf("parsed %d nonces, want %d", len(parsed), len(nonces))
	}
	for i, n := range parsed {
		if n != nonces[i] {
			t.Errorf("nonce[%d]=%d, want %d", i, n, nonces[i])
		}
	}
}

func TestNACKTracker_RateLimit(t *testing.T) {
	nt := &nackTracker{}

	nt.AddMissing(1)
	nt.AddMissing(2)

	// First NACK should succeed
	nack := nt.GenerateNACK()
	if nack == nil {
		t.Fatal("first NACK should succeed")
	}

	// Immediate second NACK should be rate-limited
	nt.AddMissing(3)
	nack = nt.GenerateNACK()
	if nack != nil {
		t.Error("second NACK should be rate-limited")
	}
}

func TestNACKTracker_Dedup(t *testing.T) {
	nt := &nackTracker{}

	nt.AddMissing(5)
	nt.AddMissing(5)
	nt.AddMissing(5)

	nack := nt.GenerateNACK()
	if nack == nil {
		t.Fatal("expected NACK")
	}

	nonces := parseNACKPacket(nack)
	if len(nonces) != 1 {
		t.Errorf("expected 1 nonce (deduped), got %d", len(nonces))
	}
}

func TestControlPacketDetection(t *testing.T) {
	// Regular FEC packet
	regular := make([]byte, 20)
	regular[0] = 0x00
	regular[1] = 0x01 // blockID = 1
	if isControlPacket(regular) {
		t.Error("regular packet should not be control")
	}

	// Control packet
	ctrl := buildNACKPacket([]uint64{1})
	if !isControlPacket(ctrl) {
		t.Error("NACK should be control packet")
	}

	// Too short
	if isControlPacket([]byte{0xFF}) {
		t.Error("too-short packet should not be control")
	}
}

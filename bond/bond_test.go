package bond

import (
	"bytes"
	"testing"
	"time"
)

func TestManager_ProcessOutboundInbound_Roundtrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReorderEnabled = false // test FEC only
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := uint32(1)

	// Send K=16 packets through outbound, collect all encoded packets
	var allEncoded [][]byte
	var originalNonces []uint64
	for i := 0; i < 16; i++ {
		pkt := []byte{byte(i), 0xDE, 0xAD}
		nonce := uint64(i)
		originalNonces = append(originalNonces, nonce)
		encoded := mgr.ProcessOutbound(peerID, pkt, nonce)
		allEncoded = append(allEncoded, encoded...)
	}

	// Feed all encoded packets through inbound — should recover all originals
	var received [][]byte
	for i, enc := range allEncoded {
		// Use the nonce from the first 16 (data packets)
		nonce := uint64(i)
		result := mgr.ProcessInbound(peerID, enc, nonce, 0)
		received = append(received, result...)
	}

	if len(received) != 16 {
		t.Fatalf("expected 16 received packets, got %d", len(received))
	}

	for i, pkt := range received {
		expected := []byte{byte(i), 0xDE, 0xAD}
		if !bytes.Equal(pkt, expected) {
			t.Errorf("packet %d: got %v, want %v", i, pkt, expected)
		}
	}
}

func TestManager_ProcessInbound_WithRecovery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReorderEnabled = false
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := uint32(1)

	// Encode 16 packets
	var allEncoded [][]byte
	for i := 0; i < 16; i++ {
		pkt := make([]byte, 20)
		pkt[0] = byte(i)
		encoded := mgr.ProcessOutbound(peerID, pkt, uint64(i))
		allEncoded = append(allEncoded, encoded...)
	}

	// Drop data packet 3, feed rest through inbound
	var received [][]byte
	for i, enc := range allEncoded {
		if i == 3 {
			continue // lost
		}
		result := mgr.ProcessInbound(peerID, enc, uint64(i), 0)
		received = append(received, result...)
	}

	// Should have all 16 (15 direct + 1 recovered)
	if len(received) != 16 {
		t.Fatalf("expected 16 packets (with recovery), got %d", len(received))
	}
}

func TestManager_ProcessInbound_WithReorder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FECEnabled = false // test reorder only
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := uint32(1)

	// Send out-of-order: 0, 2, 1
	r0 := mgr.ProcessInbound(peerID, []byte{0x45, 0}, 0, 0)
	if len(r0) != 1 {
		t.Fatalf("packet 0: expected 1 result, got %d", len(r0))
	}

	r2 := mgr.ProcessInbound(peerID, []byte{0x45, 2}, 2, 0)
	if len(r2) != 0 {
		t.Fatalf("packet 2: expected 0 results (buffered), got %d", len(r2))
	}

	r1 := mgr.ProcessInbound(peerID, []byte{0x45, 1}, 1, 0)
	if len(r1) != 2 {
		t.Fatalf("packet 1: expected 2 results (1 + flushed 2), got %d", len(r1))
	}
}

func TestManager_PerPeerIsolation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReorderEnabled = false
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Two peers encoding independently
	peer1 := uint32(1)
	peer2 := uint32(2)

	// Peer 1 sends 16 packets
	var peer1Encoded [][]byte
	for i := 0; i < 16; i++ {
		pkt := []byte{1, byte(i)} // peer 1 marker
		encoded := mgr.ProcessOutbound(peer1, pkt, uint64(i))
		peer1Encoded = append(peer1Encoded, encoded...)
	}

	// Peer 2 sends 16 packets
	var peer2Encoded [][]byte
	for i := 0; i < 16; i++ {
		pkt := []byte{2, byte(i)} // peer 2 marker
		encoded := mgr.ProcessOutbound(peer2, pkt, uint64(i))
		peer2Encoded = append(peer2Encoded, encoded...)
	}

	// Both should have 18 packets (16 data + 2 parity)
	if len(peer1Encoded) != 18 {
		t.Errorf("peer1: %d encoded, want 18", len(peer1Encoded))
	}
	if len(peer2Encoded) != 18 {
		t.Errorf("peer2: %d encoded, want 18", len(peer2Encoded))
	}

	// Decode peer 1's packets (drop one) — should recover
	var peer1Received [][]byte
	for i, enc := range peer1Encoded {
		if i == 5 {
			continue
		}
		result := mgr.ProcessInbound(peer1, enc, uint64(i), 0)
		peer1Received = append(peer1Received, result...)
	}

	if len(peer1Received) != 16 {
		t.Errorf("peer1: %d received, want 16", len(peer1Received))
	}

	// Verify peer 1's data is not contaminated by peer 2
	for _, pkt := range peer1Received {
		if len(pkt) >= 1 && pkt[0] != 1 {
			t.Errorf("peer1 received packet with wrong marker: %d", pkt[0])
		}
	}
}

func TestManager_ControlPacketNotDelivered(t *testing.T) {
	cfg := DefaultConfig()
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := uint32(1)

	// Build a NACK control packet
	nack := buildNACKPacket([]uint64{1, 2, 3})

	// ProcessInbound should return nil (control packets are not data)
	result := mgr.ProcessInbound(peerID, nack, 0, 0)
	if len(result) != 0 {
		t.Errorf("control packet should not produce output, got %d", len(result))
	}
}

func TestManager_ARQRetransmit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FECEnabled = false
	cfg.ReorderEnabled = false
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := uint32(1)

	// Store some packets via ProcessOutbound
	for i := 0; i < 10; i++ {
		mgr.ProcessOutbound(peerID, []byte{byte(i), 0xBE, 0xEF}, uint64(i))
	}

	// Set up send func to capture retransmitted packets
	var retransmitted [][]byte
	mgr.SetPeerSendFunc(peerID, func(data []byte) {
		cp := make([]byte, len(data))
		copy(cp, data)
		retransmitted = append(retransmitted, cp)
	})

	// Simulate receiving a NACK for nonces 3 and 7
	nack := buildNACKPacket([]uint64{3, 7})
	mgr.ProcessInbound(peerID, nack, 0, 0)

	if len(retransmitted) != 2 {
		t.Fatalf("expected 2 retransmissions, got %d", len(retransmitted))
	}

	// Verify retransmitted data
	if retransmitted[0][0] != 3 || retransmitted[1][0] != 7 {
		t.Errorf("retransmitted wrong packets: [%d, %d]", retransmitted[0][0], retransmitted[1][0])
	}
}

func TestManager_ProbeEcho(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FECEnabled = false
	cfg.ReorderEnabled = false
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := uint32(1)

	var sent [][]byte
	mgr.SetPeerSendFunc(peerID, func(data []byte) {
		cp := make([]byte, len(data))
		copy(cp, data)
		sent = append(sent, cp)
	})

	// Send a probe
	probe := buildProbePacket(0)
	mgr.ProcessInbound(peerID, probe, 0, 0)

	// Should have echoed it
	if len(sent) != 1 {
		t.Fatalf("expected 1 echo, got %d", len(sent))
	}

	// Echo should be a control packet with type ECHO
	echo := sent[0]
	if !isControlPacket(echo) {
		t.Error("echo should be a control packet")
	}
	if echo[2] != controlTypeEcho {
		t.Errorf("echo type=%d, want %d", echo[2], controlTypeEcho)
	}

	// Feed echo back — should update RTT
	mgr.ProcessInbound(peerID, echo, 0, 0)

	// Check path stats
	stats := mgr.GetStats()
	if len(stats.Paths) == 0 {
		t.Fatal("expected path stats after echo")
	}

	// RTT should be very small (same process)
	if stats.Paths[0].RTT > 100*time.Millisecond {
		t.Errorf("RTT=%v, expected <100ms for local echo", stats.Paths[0].RTT)
	}
}

func TestManager_Stats(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FECEnabled = false
	cfg.ReorderEnabled = false
	mgr, err := NewManager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	peerID := uint32(1)

	for i := 0; i < 5; i++ {
		mgr.ProcessOutbound(peerID, []byte{byte(i)}, uint64(i))
	}
	for i := 0; i < 3; i++ {
		mgr.ProcessInbound(peerID, []byte{byte(i)}, uint64(i), 0)
	}

	stats := mgr.GetStats()
	if stats.TxPackets != 5 {
		t.Errorf("TxPackets=%d, want 5", stats.TxPackets)
	}
	if stats.RxPackets != 3 {
		t.Errorf("RxPackets=%d, want 3", stats.RxPackets)
	}
}

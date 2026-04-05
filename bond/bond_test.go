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
	mgr.Start()
	defer mgr.Stop()

	peerID := uint32(1)
	K := cfg.FECLowK // 8

	// Send K packets through outbound
	var allEncoded [][]byte
	for i := 0; i < K; i++ {
		pkt := []byte{byte(i), 0xDE, 0xAD}
		encoded := mgr.ProcessOutbound(peerID, pkt, uint64(i))
		allEncoded = append(allEncoded, encoded...)
	}

	// Feed all through inbound
	var received [][]byte
	for i, enc := range allEncoded {
		result := mgr.ProcessInbound(peerID, enc, uint64(i), 0)
		received = append(received, result...)
	}

	if len(received) != K {
		t.Fatalf("expected %d received packets, got %d", K, len(received))
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
	K := cfg.FECLowK // 8

	var allEncoded [][]byte
	for i := 0; i < K; i++ {
		pkt := make([]byte, 20)
		pkt[0] = byte(i)
		encoded := mgr.ProcessOutbound(peerID, pkt, uint64(i))
		allEncoded = append(allEncoded, encoded...)
	}

	// Drop data packet 3, feed rest through inbound
	var received [][]byte
	for i, enc := range allEncoded {
		if i == 3 {
			continue
		}
		result := mgr.ProcessInbound(peerID, enc, uint64(i), 0)
		received = append(received, result...)
	}

	if len(received) != K {
		t.Fatalf("expected %d packets (with recovery), got %d", K, len(received))
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

	peer1 := uint32(1)
	peer2 := uint32(2)
	K := cfg.FECLowK // 8
	expectedTotal := K + cfg.FECLowM // 10

	var peer1Encoded [][]byte
	for i := 0; i < K; i++ {
		pkt := []byte{1, byte(i)}
		encoded := mgr.ProcessOutbound(peer1, pkt, uint64(i))
		peer1Encoded = append(peer1Encoded, encoded...)
	}

	var peer2Encoded [][]byte
	for i := 0; i < K; i++ {
		pkt := []byte{2, byte(i)}
		encoded := mgr.ProcessOutbound(peer2, pkt, uint64(i))
		peer2Encoded = append(peer2Encoded, encoded...)
	}

	if len(peer1Encoded) != expectedTotal {
		t.Errorf("peer1: %d encoded, want %d", len(peer1Encoded), expectedTotal)
	}
	if len(peer2Encoded) != expectedTotal {
		t.Errorf("peer2: %d encoded, want %d", len(peer2Encoded), expectedTotal)
	}

	// Decode peer 1's packets (drop one) — should recover
	var peer1Received [][]byte
	for i, enc := range peer1Encoded {
		if i == 3 {
			continue
		}
		result := mgr.ProcessInbound(peer1, enc, uint64(i), 0)
		peer1Received = append(peer1Received, result...)
	}

	if len(peer1Received) != K {
		t.Errorf("peer1: %d received, want %d", len(peer1Received), K)
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

	// Store some packets via ProcessOutbound (keyed by nonce when FEC disabled)
	for i := 0; i < 10; i++ {
		mgr.ProcessOutbound(peerID, []byte{byte(i), 0xBE, 0xEF}, uint64(i))
	}

	// Capture retransmitted packets (now RETRANSMIT control packets)
	var retransmitted [][]byte
	mgr.SetPeerSendFunc(peerID, func(data []byte) {
		cp := make([]byte, len(data))
		copy(cp, data)
		retransmitted = append(retransmitted, cp)
	})

	// NACK for nonces 3 and 7
	nack := buildNACKPacket([]uint64{3, 7})
	mgr.ProcessInbound(peerID, nack, 0, 0)

	if len(retransmitted) != 2 {
		t.Fatalf("expected 2 retransmissions, got %d", len(retransmitted))
	}

	// Retransmitted packets are RETRANSMIT control packets — parse them
	for i, pkt := range retransmitted {
		if !isControlPacket(pkt) {
			t.Errorf("retransmit %d: not a control packet", i)
			continue
		}
		seq, payload := parseRetransmitPacket(pkt)
		if payload == nil {
			t.Errorf("retransmit %d: nil payload", i)
			continue
		}
		expectedSeq := []uint64{3, 7}[i]
		if seq != expectedSeq {
			t.Errorf("retransmit %d: seq=%d, want %d", i, seq, expectedSeq)
		}
		if payload[0] != byte(expectedSeq) {
			t.Errorf("retransmit %d: data[0]=%d, want %d", i, payload[0], expectedSeq)
		}
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

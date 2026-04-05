package bond

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// testConfig returns a config with explicit K=8, M=2 for predictable tests.
func testConfig() Config {
	cfg := DefaultConfig()
	cfg.FECLowK = 8
	cfg.FECLowM = 2
	return cfg
}

func TestFECEncoderDecoder_Roundtrip(t *testing.T) {
	cfg := testConfig() // K=8, M=2
	enc, err := NewFECEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder(200, 256)

	// Send K=8 packets through encoder
	var encoded [][]byte
	for i := 0; i < 8; i++ {
		pkt := []byte{byte(i), 0xAA, 0xBB, 0xCC}
		data, parity, _ := enc.Encode(pkt, uint64(100+i))
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Should have 8 data + 2 parity = 10 packets
	if len(encoded) != 10 {
		t.Fatalf("expected 10 encoded packets, got %d", len(encoded))
	}

	// Decode all — should get payloads back with sequential dataSeq
	for i := 0; i < 8; i++ {
		data, _ := dec.Decode(encoded[i])
		if data == nil {
			t.Fatalf("packet %d: expected data, got nil", i)
		}
		if data.DataSeq != uint64(i) {
			t.Errorf("packet %d: dataSeq=%d, want %d", i, data.DataSeq, i)
		}
		expected := []byte{byte(i), 0xAA, 0xBB, 0xCC}
		if !bytes.Equal(data.Data, expected) {
			t.Errorf("packet %d: data=%v, want %v", i, data.Data, expected)
		}
	}
}

func TestFECRecovery_SingleLoss(t *testing.T) {
	cfg := testConfig() // K=8, M=2
	enc, err := NewFECEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder(200, 256)

	// Encode 8 packets
	var encoded [][]byte
	for i := 0; i < 8; i++ {
		pkt := make([]byte, 100)
		pkt[0] = byte(i)
		data, parity, _ := enc.Encode(pkt, uint64(i))
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Drop packet 3 (data index 3), feed rest to decoder
	lostIdx := 3
	var recovered []*DecodedPacket
	for i, pkt := range encoded {
		if i == lostIdx {
			continue
		}
		_, rec := dec.Decode(pkt)
		recovered = append(recovered, rec...)
	}

	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered packet, got %d", len(recovered))
	}
	if recovered[0].DataSeq != uint64(lostIdx) {
		t.Errorf("recovered dataSeq=%d, want %d", recovered[0].DataSeq, lostIdx)
	}
	if recovered[0].Data[0] != byte(lostIdx) {
		t.Errorf("recovered data[0]=%d, want %d", recovered[0].Data[0], lostIdx)
	}
}

func TestFECRecovery_TwoLoss(t *testing.T) {
	cfg := testConfig() // K=8, M=2 — can recover exactly 2
	enc, err := NewFECEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder(200, 256)

	var encoded [][]byte
	for i := 0; i < 8; i++ {
		pkt := make([]byte, 50)
		pkt[0] = byte(i)
		data, parity, _ := enc.Encode(pkt, uint64(i))
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Drop packets 2 and 5 (both in same block)
	drop := map[int]bool{2: true, 5: true}
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
	seqs := map[uint64]bool{}
	for _, r := range recovered {
		seqs[r.DataSeq] = true
	}
	if !seqs[2] || !seqs[5] {
		t.Errorf("expected dataSeqs 2 and 5, got %v", seqs)
	}
}

func TestFECRecovery_TooManyLost(t *testing.T) {
	cfg := testConfig() // K=8, M=2 — 3 lost in same block is unrecoverable
	enc, err := NewFECEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dec := NewFECDecoder(200, 256)

	var encoded [][]byte
	for i := 0; i < 8; i++ {
		pkt := []byte{byte(i)}
		data, parity, _ := enc.Encode(pkt, uint64(i))
		encoded = append(encoded, data)
		if parity != nil {
			encoded = append(encoded, parity...)
		}
	}

	// Drop 3 data packets from the same block (indices 1, 4, 6)
	drop := map[int]bool{1: true, 4: true, 6: true}
	var recovered []*DecodedPacket
	for i, pkt := range encoded {
		if drop[i] {
			continue
		}
		_, rec := dec.Decode(pkt)
		recovered = append(recovered, rec...)
	}

	if len(recovered) != 0 {
		t.Errorf("expected 0 recovered packets with 3 lost (M=2), got %d", len(recovered))
	}
}

func TestFECKeepaliveBypass(t *testing.T) {
	enc, err := NewFECEncoder(testConfig())
	if err != nil {
		t.Fatal(err)
	}

	data, parity, _ := enc.Encode([]byte{}, 0)
	if len(data) != FECPayloadOffset {
		t.Errorf("encoded empty packet length=%d, want %d", len(data), FECPayloadOffset)
	}
	if parity != nil {
		t.Error("unexpected parity from single packet")
	}
}

func TestFECHeaderFormat(t *testing.T) {
	cfg := testConfig() // K=8, M=2
	enc, err := NewFECEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}

	pkt := []byte{0x45, 0x00, 0x00, 0x1c}
	data, _, seq := enc.Encode(pkt, 12345)

	blockID := binary.BigEndian.Uint16(data[0:2])
	index := data[2]
	k := data[3]
	m := data[4]
	embeddedSeq := binary.BigEndian.Uint64(data[FECHeaderSize:FECPayloadOffset])

	if blockID != 0 {
		t.Errorf("blockID=%d, want 0", blockID)
	}
	if index != 0 {
		t.Errorf("index=%d, want 0", index)
	}
	if k != 8 {
		t.Errorf("K=%d, want 8", k)
	}
	if m != 2 {
		t.Errorf("M=%d, want 2", m)
	}
	if embeddedSeq != seq {
		t.Errorf("embedded seq=%d, want %d", embeddedSeq, seq)
	}
	if !bytes.Equal(data[FECPayloadOffset:], pkt) {
		t.Errorf("payload mismatch")
	}
}

func TestFECControlPacketNotDecoded(t *testing.T) {
	dec := NewFECDecoder(100, 256)
	pkt := make([]byte, 20)
	binary.BigEndian.PutUint16(pkt[0:2], controlBlockID)
	pkt[2] = controlTypeNACK

	data, recovered := dec.Decode(pkt)
	if data != nil || recovered != nil {
		t.Error("control packet should not produce decoded data")
	}
}

func TestFECDataSeqContiguous(t *testing.T) {
	// Verify dataSeq increments only for data packets, never for parity
	cfg := testConfig() // K=8, M=2
	enc, err := NewFECEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var seqs []uint64
	for i := 0; i < 16; i++ {
		_, _, seq := enc.Encode([]byte{byte(i)}, uint64(100+i))
		seqs = append(seqs, seq)
	}

	// dataSeq should be 0,1,2,...,15 (contiguous, no gaps from parity)
	for i, seq := range seqs {
		if seq != uint64(i) {
			t.Errorf("dataSeq[%d]=%d, want %d (parity gap?)", i, seq, i)
		}
	}
}

func TestFECBlockCap(t *testing.T) {
	dec := NewFECDecoder(100, 4) // max 4 concurrent blocks

	// Create 6 blocks — should cap at 4, evicting oldest
	for blockID := uint16(0); blockID < 6; blockID++ {
		pkt := make([]byte, FECPayloadOffset+10)
		binary.BigEndian.PutUint16(pkt[0:2], blockID)
		pkt[2] = 0 // index 0
		pkt[3] = 8 // K=8
		pkt[4] = 2 // M=2
		dec.Decode(pkt)
	}

	dec.mu.Lock()
	n := len(dec.blocks)
	dec.mu.Unlock()

	if n > 4 {
		t.Errorf("block count=%d, should be capped at 4", n)
	}
}

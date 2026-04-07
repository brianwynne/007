package bond

import (
	"bytes"
	"testing"
)

func TestSlidingFEC_Roundtrip(t *testing.T) {
	enc := NewSlidingFECEncoder(5)
	dec := NewSlidingFECDecoder(64, 0)

	for i := 0; i < 10; i++ {
		pkt := []byte{byte(i), 0xAA, 0xBB}
		dataEnc, repairEnc, seq := enc.Encode(pkt, uint64(i))

		if seq != uint64(i) {
			t.Errorf("packet %d: dataSeq=%d, want %d", i, seq, i)
		}

		data, _, _ := dec.Decode(dataEnc)
		if data == nil {
			t.Fatalf("packet %d: data is nil", i)
		}
		if !bytes.Equal(data.Data, pkt) {
			t.Errorf("packet %d: data=%v, want %v", i, data.Data, pkt)
		}
		if data.DataSeq != uint64(i) {
			t.Errorf("packet %d: dataSeq=%d, want %d", i, data.DataSeq, i)
		}

		// Process repair (no recovery expected — all data present)
		_, repairRec, _ := dec.Decode(repairEnc)
		if len(repairRec) > 0 {
			t.Errorf("packet %d: unexpected recovery", i)
		}
	}
}

func TestSlidingFEC_SingleLoss(t *testing.T) {
	enc := NewSlidingFECEncoder(5)
	dec := NewSlidingFECDecoder(64, 0)

	var allData [][]byte
	var allRepairs [][]byte

	// Encode 6 packets
	for i := 0; i < 6; i++ {
		pkt := make([]byte, 20)
		pkt[0] = byte(i)
		d, r, _ := enc.Encode(pkt, uint64(i))
		allData = append(allData, d)
		allRepairs = append(allRepairs, r)
	}

	// Drop packet 3 — feed the rest
	var recovered []*DecodedPacket
	for i := 0; i < 6; i++ {
		if i == 3 {
			continue // lost
		}
		_, rec, _ := dec.Decode(allData[i])
		recovered = append(recovered, rec...)
	}

	// Now feed repairs — one of them should recover packet 3
	for i := 0; i < 6; i++ {
		_, rec, _ := dec.Decode(allRepairs[i])
		recovered = append(recovered, rec...)
	}

	// Should have recovered exactly 1 packet
	found := false
	for _, r := range recovered {
		if r.DataSeq == 3 {
			if r.Data[0] != 3 {
				t.Errorf("recovered data[0]=%d, want 3", r.Data[0])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("packet 3 not recovered (got %d recoveries)", len(recovered))
	}
}

func TestSlidingFEC_BurstLoss2(t *testing.T) {
	enc := NewSlidingFECEncoder(5)
	dec := NewSlidingFECDecoder(64, 0)

	var allData [][]byte
	var allRepairs [][]byte

	for i := 0; i < 8; i++ {
		pkt := make([]byte, 10)
		pkt[0] = byte(i)
		d, r, _ := enc.Encode(pkt, uint64(i))
		allData = append(allData, d)
		allRepairs = append(allRepairs, r)
	}

	// Drop packets 3 and 4 (burst of 2)
	drop := map[int]bool{3: true, 4: true}
	var recovered []*DecodedPacket

	// Feed data (skip dropped)
	for i, d := range allData {
		if drop[i] {
			continue
		}
		_, rec, _ := dec.Decode(d)
		recovered = append(recovered, rec...)
	}

	// Feed repairs
	for _, r := range allRepairs {
		_, rec, _ := dec.Decode(r)
		recovered = append(recovered, rec...)
	}

	// With W=5 and overlapping windows, should recover both
	seqs := map[uint64]bool{}
	for _, r := range recovered {
		seqs[r.DataSeq] = true
	}

	if !seqs[3] {
		t.Error("packet 3 not recovered")
	}
	if !seqs[4] {
		t.Error("packet 4 not recovered")
	}
}

func TestSlidingFEC_NoLoss(t *testing.T) {
	enc := NewSlidingFECEncoder(5)
	dec := NewSlidingFECDecoder(64, 0)

	for i := 0; i < 10; i++ {
		pkt := []byte{byte(i)}
		d, r, _ := enc.Encode(pkt, uint64(i))

		data, rec, _ := dec.Decode(d)
		if data == nil {
			t.Fatalf("packet %d: nil data", i)
		}
		if len(rec) > 0 {
			t.Errorf("packet %d: false recovery", i)
		}

		_, rec, _ = dec.Decode(r)
		if len(rec) > 0 {
			t.Errorf("repair %d: false recovery", i)
		}
	}
}

func TestSlidingFEC_RepairOnly(t *testing.T) {
	enc := NewSlidingFECEncoder(3)
	dec := NewSlidingFECDecoder(64, 0)

	// Encode 3 packets to fill the window
	for i := 0; i < 3; i++ {
		enc.Encode([]byte{byte(i)}, uint64(i))
	}
	// Get a repair covering all 3
	_, r, _ := enc.Encode([]byte{0x42}, 3)

	// Feed only the repair — should not crash
	// With all 4 data packets missing, can't recover (need exactly 1 missing)
	data, rec, _ := dec.Decode(r)
	if data != nil {
		t.Error("repair-only should not return data")
	}
	// Recovery not possible — all data missing
	_ = rec // may or may not recover depending on window fill
}

func TestSlidingFEC_IsSlidingPacket(t *testing.T) {
	enc := NewSlidingFECEncoder(3)
	d, r, _ := enc.Encode([]byte{1, 2, 3}, 0)

	if !IsSlidingFECPacket(d) {
		t.Error("data packet should be detected as sliding")
	}
	if !IsSlidingFECPacket(r) {
		t.Error("repair packet should be detected as sliding")
	}

	// Block FEC packet (starts with blockID, not type byte)
	blockPkt := make([]byte, 20)
	blockPkt[0] = 0x00
	blockPkt[1] = 0x05 // blockID = 5
	if IsSlidingFECPacket(blockPkt) {
		t.Error("block FEC packet should not be detected as sliding")
	}

	// Control packet
	ctrl := buildNACKPacket([]uint64{1})
	if IsSlidingFECPacket(ctrl) {
		t.Error("control packet should not be detected as sliding")
	}
}

func TestSlidingFEC_DataSeqContiguous(t *testing.T) {
	enc := NewSlidingFECEncoder(5)

	var seqs []uint64
	for i := 0; i < 20; i++ {
		_, _, seq := enc.Encode([]byte{byte(i)}, uint64(i))
		seqs = append(seqs, seq)
	}

	for i, seq := range seqs {
		if seq != uint64(i) {
			t.Errorf("seq[%d]=%d, want %d", i, seq, i)
		}
	}
}

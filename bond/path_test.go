package bond

import (
	"testing"
	"time"
)

func TestPathTracker_RecordReceive(t *testing.T) {
	pt := newPathTracker()

	for i := 0; i < 10; i++ {
		pt.RecordReceive(0)
	}
	for i := 0; i < 5; i++ {
		pt.RecordReceive(1)
	}

	paths := pt.GetAll()
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}

	counts := map[int]uint64{}
	for _, p := range paths {
		counts[p.PathID] = p.RxCount
	}
	if counts[0] != 10 {
		t.Errorf("path 0 rx=%d, want 10", counts[0])
	}
	if counts[1] != 5 {
		t.Errorf("path 1 rx=%d, want 5", counts[1])
	}
}

func TestPathTracker_UpdateRTT(t *testing.T) {
	pt := newPathTracker()

	pt.UpdateRTT(0, 50*time.Millisecond)
	pt.UpdateRTT(0, 60*time.Millisecond)
	pt.UpdateRTT(0, 55*time.Millisecond)

	paths := pt.GetAll()
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}

	// RTT should be close to 50-60ms range (EWMA)
	if paths[0].RTT < 40*time.Millisecond || paths[0].RTT > 70*time.Millisecond {
		t.Errorf("RTT=%v, expected 40-70ms range", paths[0].RTT)
	}
	if paths[0].RTTVar == 0 {
		t.Error("RTTVar should be non-zero")
	}
}

func TestPathTracker_LossTracking(t *testing.T) {
	pt := newPathTracker()

	// 8 received, 2 lost
	for i := 0; i < 8; i++ {
		pt.RecordReceive(0)
	}
	for i := 0; i < 2; i++ {
		pt.RecordLoss(0)
	}

	paths := pt.GetAll()
	if len(paths) != 1 {
		t.Fatal("expected 1 path")
	}

	// Loss should be 2/10 = 0.2
	if paths[0].Loss < 0.15 || paths[0].Loss > 0.25 {
		t.Errorf("loss=%f, want ~0.2", paths[0].Loss)
	}
}

func TestProbeEchoPackets(t *testing.T) {
	probe := buildProbePacket(42)

	if !isControlPacket(probe) {
		t.Error("probe should be a control packet")
	}
	if probe[2] != controlTypeProbe {
		t.Errorf("probe type=%d, want %d", probe[2], controlTypeProbe)
	}

	ts, pathID, ok := parseProbeEcho(probe)
	if !ok {
		t.Fatal("failed to parse probe")
	}
	if pathID != 42 {
		t.Errorf("pathID=%d, want 42", pathID)
	}
	if ts == 0 {
		t.Error("timestamp should be non-zero")
	}

	// Echo from probe
	echo := buildEchoPacket(probe)
	if echo == nil {
		t.Fatal("echo should not be nil")
	}
	if echo[2] != controlTypeEcho {
		t.Errorf("echo type=%d, want %d", echo[2], controlTypeEcho)
	}

	ts2, pathID2, ok := parseProbeEcho(echo)
	if !ok {
		t.Fatal("failed to parse echo")
	}
	if ts2 != ts {
		t.Error("echo should preserve probe timestamp")
	}
	if pathID2 != pathID {
		t.Error("echo should preserve probe pathID")
	}
}

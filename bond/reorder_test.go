package bond

import (
	"testing"
	"time"
)

func TestReorderBuffer_InOrder(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())

	for i := uint64(0); i < 10; i++ {
		data := []byte{byte(i)}
		result := rb.Insert(data, i, 0)
		if len(result) != 1 {
			t.Fatalf("nonce %d: expected 1 result, got %d", i, len(result))
		}
		if result[0][0] != byte(i) {
			t.Errorf("nonce %d: data=%d, want %d", i, result[0][0], i)
		}
	}

	inOrder, reordered, gaps, _ := rb.Stats()
	if inOrder != 10 {
		t.Errorf("inOrder=%d, want 10", inOrder)
	}
	if reordered != 0 {
		t.Errorf("reordered=%d, want 0", reordered)
	}
	if gaps != 0 {
		t.Errorf("gaps=%d, want 0", gaps)
	}
}

func TestReorderBuffer_OutOfOrder(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())

	// Send packet 0
	result := rb.Insert([]byte{0}, 0, 0)
	if len(result) != 1 {
		t.Fatalf("nonce 0: expected 1 result, got %d", len(result))
	}

	// Skip packet 1, send packet 2
	result = rb.Insert([]byte{2}, 2, 0)
	if len(result) != 0 {
		t.Fatalf("nonce 2: expected 0 results (buffered), got %d", len(result))
	}

	// Now send packet 1 — should release both 1 and 2
	result = rb.Insert([]byte{1}, 1, 0)
	if len(result) != 2 {
		t.Fatalf("nonce 1: expected 2 results, got %d", len(result))
	}
	if result[0][0] != 1 || result[1][0] != 2 {
		t.Errorf("expected [1, 2], got [%d, %d]", result[0][0], result[1][0])
	}
}

func TestReorderBuffer_GapTimeout(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())
	// Set very short window for testing
	rb.mu.Lock()
	rb.maxWindow = 10 * time.Millisecond
	rb.minWindow = 5 * time.Millisecond
	rb.mu.Unlock()

	// Send packet 0
	rb.Insert([]byte{0}, 0, 0)

	// Skip packet 1, send packet 2
	rb.Insert([]byte{2}, 2, 0)

	// Wait for gap timeout
	time.Sleep(20 * time.Millisecond)

	// Send packet 3 — should trigger gap timeout and release 2, 3
	result := rb.Insert([]byte{3}, 3, 0)
	if len(result) < 1 {
		t.Fatalf("expected at least 1 result after gap timeout, got %d", len(result))
	}

	_, _, gaps, _ := rb.Stats()
	if gaps != 1 {
		t.Errorf("gaps=%d, want 1", gaps)
	}
}

func TestReorderBuffer_Duplicate(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())

	result := rb.Insert([]byte{0}, 0, 0)
	if len(result) != 1 {
		t.Fatal("first insert should return 1")
	}

	// Duplicate — should be ignored
	result = rb.Insert([]byte{0}, 0, 0)
	if len(result) != 0 {
		t.Errorf("duplicate should return 0, got %d", len(result))
	}
}

func TestReorderBuffer_LatePacket(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())

	// Deliver 0, 1, 2
	for i := uint64(0); i < 3; i++ {
		rb.Insert([]byte{byte(i)}, i, 0)
	}

	// Late packet 0 — already delivered
	result := rb.Insert([]byte{0}, 0, 0)
	if len(result) != 0 {
		t.Errorf("late packet should return 0, got %d", len(result))
	}
}

func TestReorderBuffer_Flush(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())
	rb.mu.Lock()
	rb.maxWindow = 5 * time.Millisecond
	rb.minWindow = 5 * time.Millisecond
	rb.mu.Unlock()

	// Send 0, skip 1, send 2
	rb.Insert([]byte{0}, 0, 0)
	rb.Insert([]byte{2}, 2, 0)

	time.Sleep(10 * time.Millisecond)

	// Flush should skip the gap
	rb.Flush()

	_, _, gaps, _ := rb.Stats()
	if gaps != 1 {
		t.Errorf("gaps=%d after flush, want 1", gaps)
	}
}

func TestReorderBuffer_SkippedNonces(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())
	rb.mu.Lock()
	rb.maxWindow = 5 * time.Millisecond
	rb.minWindow = 5 * time.Millisecond
	rb.mu.Unlock()

	rb.Insert([]byte{0}, 0, 0)
	rb.Insert([]byte{3}, 3, 0) // skip 1, 2

	time.Sleep(10 * time.Millisecond)

	rb.Insert([]byte{4}, 4, 0) // triggers gap timeout

	skipped := rb.DrainSkippedNonces()
	if len(skipped) != 2 {
		t.Fatalf("expected 2 skipped nonces, got %d", len(skipped))
	}
	if skipped[0] != 1 || skipped[1] != 2 {
		t.Errorf("skipped=%v, want [1, 2]", skipped)
	}

	// Second drain should be empty
	skipped = rb.DrainSkippedNonces()
	if len(skipped) != 0 {
		t.Errorf("second drain should be empty, got %d", len(skipped))
	}
}

func TestReorderBuffer_LargeGap(t *testing.T) {
	rb := NewReorderBuffer(DefaultConfig())

	// Send packets 0-4
	for i := uint64(0); i < 5; i++ {
		rb.Insert([]byte{byte(i)}, i, 0)
	}

	// Jump to nonce 100 — big gap
	result := rb.Insert([]byte{100}, 100, 0)
	if len(result) != 0 {
		t.Fatalf("big gap should buffer, got %d results", len(result))
	}
}

/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

package bond

import (
	"sync"
	"time"
)

// ReorderBuffer delivers packets in sequence order despite multi-path
// arrival timing differences. Uses a ring buffer with per-path timeout
// awareness and adaptive window sizing.
//
// Design decisions:
//   - Ring buffer for O(1) insert/remove, cache-friendly, zero-alloc after init
//   - Synchronous API: Insert returns packets ready for delivery
//   - Gap timeout checked on each Insert call + periodic Flush
//   - Per-path timeout: based on measured RTT + variance
//   - Adaptive window: increases on gaps (10%), decreases on stability (5% per 5s)
//   - Uses WireGuard nonce (64-bit) as sequence number — no new header needed
type ReorderBuffer struct {
	mu sync.Mutex

	// Ring buffer
	slots       [maxBufferSize]*bufferedPacket
	nextExpect  uint64 // next expected sequence number
	initialized bool

	// Gap tracking — when did we first buffer a future packet
	gapStart  time.Time
	gapPathID int

	// Timing
	maxWindow   time.Duration // current max wait before skipping gap
	minWindow   time.Duration // minimum allowed window
	lastGapTime time.Time     // when last gap was detected (for adaptive decrease)

	// Per-path latency tracking
	pathRTT map[int]*PathStats

	// Stats
	inOrderCount   uint64
	reorderedCount uint64
	gapCount       uint64
	duplicateCount uint64
	lateCount      uint64

	// Skipped nonces for ARQ NACK generation
	skippedNonces []uint64

	// Packets released by background Flush, delivered on next InsertAt
	pendingFlush [][]byte
}

// bufferedPacket holds a packet waiting for in-order delivery.
type bufferedPacket struct {
	data    []byte
	nonce   uint64
	pathID  int
	arrival time.Time
}

// PathStats tracks per-path latency for intelligent timeout calculation.
type PathStats struct {
	RTT       time.Duration // exponential moving average
	RTTVar    time.Duration // RTT variance
	LastSeen  time.Time
	PacketCnt uint64
}

const (
	maxBufferSize     = 64                  // max packets in reorder buffer
	defaultWindowMs   = 80                  // default reorder window (ms)
	minWindowMs       = 20                  // minimum window (ms)
	maxWindowMs       = 200                 // maximum window (ms)
	adaptIncreaseRate = 1.10                // increase window by 10% on gap
	adaptDecreaseRate = 0.95                // decrease window by 5% on stability
	stabilityTimeout  = 5 * time.Second     // decrease after this much stability
)

// NewReorderBuffer creates a new adaptive reorder buffer.
func NewReorderBuffer(cfg Config) *ReorderBuffer {
	windowMs := cfg.ReorderWindowMs
	if windowMs == 0 {
		windowMs = defaultWindowMs
	}
	minMs := cfg.ReorderMinMs
	if minMs == 0 {
		minMs = minWindowMs
	}
	return &ReorderBuffer{
		maxWindow:   time.Duration(windowMs) * time.Millisecond,
		minWindow:   time.Duration(minMs) * time.Millisecond,
		pathRTT:     make(map[int]*PathStats),
		lastGapTime: time.Now(),
	}
}

// Insert adds a packet to the reorder buffer and returns any packets
// ready for in-order delivery. Returns nil if the packet is buffered.
//
// Ready packets are returned when:
//   - The packet is the next expected in sequence (immediate delivery)
//   - Buffered consecutive packets follow it (flushed)
//   - A gap has timed out (skipped, buffered packets flushed)
func (rb *ReorderBuffer) Insert(data []byte, nonce uint64, pathID int) [][]byte {
	return rb.InsertAt(data, nonce, pathID, time.Now())
}

// InsertAt is like Insert but accepts a pre-computed timestamp to avoid
// redundant time.Now() calls when processing multiple packets.
func (rb *ReorderBuffer) InsertAt(data []byte, nonce uint64, pathID int, now time.Time) [][]byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Drain any packets released by background Flush
	var result [][]byte
	if len(rb.pendingFlush) > 0 {
		result = append(result, rb.pendingFlush...)
		rb.pendingFlush = nil
	}

	// Update path stats
	ps := rb.getPathStats(pathID)
	ps.LastSeen = now
	ps.PacketCnt++

	// First packet — initialize expected sequence
	if !rb.initialized {
		rb.nextExpect = nonce
		rb.initialized = true
	}

	// Check gap timeout before processing new packet
	result = append(result, rb.checkGapTimeout(now)...)

	// Duplicate or late — already delivered or skipped
	if nonce < rb.nextExpect {
		rb.lateCount++
		return result
	}

	// Expected packet — deliver immediately (take ownership, no copy)
	if nonce == rb.nextExpect {
		result = append(result, data)
		rb.nextExpect++
		rb.inOrderCount++

		// Deliver any buffered consecutive packets
		result = append(result, rb.flushConsecutive()...)

		// Gap resolved
		rb.gapStart = time.Time{}
		return result
	}

	// Future packet — buffer it
	idx := nonce % maxBufferSize
	if rb.slots[idx] != nil && rb.slots[idx].nonce == nonce {
		rb.duplicateCount++
		return result // duplicate
	}

	rb.slots[idx] = &bufferedPacket{
		data:    data, // take ownership — caller (FEC decode) already allocated
		nonce:   nonce,
		pathID:  pathID,
		arrival: now,
	}
	rb.reorderedCount++

	// Start gap timer if this is the first future packet
	if rb.gapStart.IsZero() {
		rb.gapStart = now
		rb.gapPathID = pathID
	}

	return result
}

// Flush checks for timed-out gaps. Packets released are stored in
// pendingFlush and delivered on the next InsertAt call.
func (rb *ReorderBuffer) Flush() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	flushed := rb.checkGapTimeout(time.Now())
	if len(flushed) > 0 {
		rb.pendingFlush = append(rb.pendingFlush, flushed...)
	}
}

// checkGapTimeout checks if we've been waiting for nextExpect too long.
// If so, skips the gap and flushes consecutive buffered packets.
// Caller must hold rb.mu.
func (rb *ReorderBuffer) checkGapTimeout(now time.Time) [][]byte {
	if rb.gapStart.IsZero() {
		return nil
	}

	timeout := rb.timeoutForPath(rb.gapPathID)
	if now.Sub(rb.gapStart) < timeout {
		return nil // not timed out yet
	}

	// Gap timed out — find the next buffered packet and skip to it
	var result [][]byte

	// Scan forward from nextExpect to find the next buffered packet
	for offset := uint64(1); offset < maxBufferSize; offset++ {
		candidate := rb.nextExpect + offset
		idx := candidate % maxBufferSize
		if rb.slots[idx] != nil && rb.slots[idx].nonce == candidate {
			// Found it — record skipped nonces for ARQ, then skip gap
			for skipped := rb.nextExpect; skipped < candidate; skipped++ {
				rb.skippedNonces = append(rb.skippedNonces, skipped)
			}
			rb.gapCount++
			rb.nextExpect = candidate

			// Adapt window — increase because we had a gap
			rb.lastGapTime = now
			newWindow := time.Duration(float64(rb.maxWindow) * adaptIncreaseRate)
			if newWindow > time.Duration(maxWindowMs)*time.Millisecond {
				newWindow = time.Duration(maxWindowMs) * time.Millisecond
			}
			rb.maxWindow = newWindow

			// Flush consecutive from the new position
			result = rb.flushConsecutive()
			rb.gapStart = time.Time{}

			// Check if there's still a gap after flushing
			nextIdx := rb.nextExpect % maxBufferSize
			if rb.slots[nextIdx] != nil && rb.slots[nextIdx].nonce > rb.nextExpect {
				// Still have buffered future packets — new gap
				rb.gapStart = now
				rb.gapPathID = rb.slots[nextIdx].pathID
			}

			return result
		}
	}

	// No buffered packets found — clear gap state
	rb.gapStart = time.Time{}
	return nil
}

// flushConsecutive delivers buffered packets starting from nextExpect.
// Caller must hold rb.mu.
func (rb *ReorderBuffer) flushConsecutive() [][]byte {
	var result [][]byte
	for {
		idx := rb.nextExpect % maxBufferSize
		pkt := rb.slots[idx]
		if pkt == nil || pkt.nonce != rb.nextExpect {
			break
		}
		result = append(result, pkt.data)
		rb.slots[idx] = nil
		rb.nextExpect++
	}
	return result
}

// timeoutForPath returns the gap timeout based on measured path latency.
// Caller must hold rb.mu.
func (rb *ReorderBuffer) timeoutForPath(pathID int) time.Duration {
	ps, ok := rb.pathRTT[pathID]
	if !ok || ps.RTT == 0 {
		return rb.maxWindow
	}
	// Timeout = path RTT + 2*variance + margin
	timeout := ps.RTT + 2*ps.RTTVar + 10*time.Millisecond
	if timeout > rb.maxWindow {
		return rb.maxWindow
	}
	if timeout < rb.minWindow {
		return rb.minWindow
	}
	return timeout
}

// getPathStats returns stats for a path, creating if needed.
// Caller must hold rb.mu.
func (rb *ReorderBuffer) getPathStats(pathID int) *PathStats {
	ps, ok := rb.pathRTT[pathID]
	if !ok {
		ps = &PathStats{}
		rb.pathRTT[pathID] = ps
	}
	return ps
}

// UpdatePathRTT updates the measured RTT for a path.
// Called with keepalive round-trip measurements.
func (rb *ReorderBuffer) UpdatePathRTT(pathID int, rtt time.Duration) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	ps := rb.getPathStats(pathID)

	if ps.RTT == 0 {
		ps.RTT = rtt
		ps.RTTVar = rtt / 2
	} else {
		// Exponential moving average (TCP-style)
		diff := ps.RTT - rtt
		if diff < 0 {
			diff = -diff
		}
		ps.RTTVar = time.Duration(0.75*float64(ps.RTTVar) + 0.25*float64(diff))
		ps.RTT = time.Duration(0.875*float64(ps.RTT) + 0.125*float64(rtt))
	}
}

// AdaptWindow periodically decreases window size when stable.
// Call this from a goroutine every second.
func (rb *ReorderBuffer) AdaptWindow() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if time.Since(rb.lastGapTime) > stabilityTimeout {
		newWindow := time.Duration(float64(rb.maxWindow) * adaptDecreaseRate)
		if newWindow < rb.minWindow {
			newWindow = rb.minWindow
		}
		rb.maxWindow = newWindow
	}
}

// Stats returns current buffer statistics.
func (rb *ReorderBuffer) Stats() (inOrder, reordered, gaps, duplicates, late uint64, windowMs int64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.inOrderCount, rb.reorderedCount, rb.gapCount, rb.duplicateCount, rb.lateCount, rb.maxWindow.Milliseconds()
}

// DrainSkippedNonces returns and clears the list of nonces that were
// skipped due to gap timeouts. Used by ARQ to generate NACKs.
func (rb *ReorderBuffer) DrainSkippedNonces() []uint64 {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if len(rb.skippedNonces) == 0 {
		return nil
	}
	nonces := rb.skippedNonces
	rb.skippedNonces = nil
	return nonces
}

// copyBytes returns a copy of the data slice.
func copyBytes(data []byte) []byte {
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp
}

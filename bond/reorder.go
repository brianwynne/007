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
//   - Per-path timeout: ethernet 20ms, WiFi 50ms, cellular 110ms
//   - Adaptive window: increases on gaps (10%), decreases on stability (5% per 5s)
//   - Uses WireGuard nonce (64-bit) as sequence number — no new header needed
type ReorderBuffer struct {
	mu sync.Mutex

	// Ring buffer
	slots      [maxBufferSize]*BufferedPacket
	nextExpect uint64 // next expected sequence number
	delivered  uint64 // last delivered sequence number

	// Timing
	maxWindow     time.Duration // current max wait before delivering
	minWindow     time.Duration // minimum allowed window
	defaultWindow time.Duration // initial window size
	lastGapTime   time.Time     // when last gap was detected

	// Per-path latency tracking
	pathRTT map[int]*PathStats

	// Output channel
	output chan *BufferedPacket

	// Stats
	inOrderCount   uint64
	reorderedCount uint64
	gapCount       uint64
}

// BufferedPacket holds a packet waiting for in-order delivery.
type BufferedPacket struct {
	Data      []byte
	Nonce     uint64    // WireGuard nonce = sequence number
	PathID    int       // which network path delivered this packet
	Arrival   time.Time // when the packet arrived
	timer     *time.Timer
}

// PathStats tracks per-path latency for intelligent timeout calculation.
type PathStats struct {
	RTT       time.Duration // exponential moving average
	RTTVar    time.Duration // RTT variance
	LastSeen  time.Time
	PacketCnt uint64
	LossCnt   uint64
}

const (
	maxBufferSize     = 64   // max packets in reorder buffer
	defaultWindowMs   = 80   // default reorder window (ms)
	minWindowMs       = 20   // minimum window (ms)
	maxWindowMs       = 200  // maximum window (ms)
	adaptIncreaseRate = 1.10 // increase window by 10% on gap
	adaptDecreaseRate = 0.95 // decrease window by 5% on stability
	stabilityTimeout  = 5 * time.Second // decrease after this much stability
)

// NewReorderBuffer creates a new adaptive reorder buffer.
func NewReorderBuffer(outputChanSize int) *ReorderBuffer {
	rb := &ReorderBuffer{
		maxWindow:     time.Duration(defaultWindowMs) * time.Millisecond,
		minWindow:     time.Duration(minWindowMs) * time.Millisecond,
		defaultWindow: time.Duration(defaultWindowMs) * time.Millisecond,
		pathRTT:       make(map[int]*PathStats),
		output:        make(chan *BufferedPacket, outputChanSize),
		lastGapTime:   time.Now(),
	}
	return rb
}

// Output returns the channel that receives in-order packets.
func (rb *ReorderBuffer) Output() <-chan *BufferedPacket {
	return rb.output
}

// Insert adds a packet to the reorder buffer. Packets are delivered
// via the Output channel in sequence order.
func (rb *ReorderBuffer) Insert(data []byte, nonce uint64, pathID int) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	now := time.Now()

	// Update path stats
	ps, ok := rb.pathRTT[pathID]
	if !ok {
		ps = &PathStats{}
		rb.pathRTT[pathID] = ps
	}
	ps.LastSeen = now
	ps.PacketCnt++

	// First packet — initialize expected sequence
	if rb.nextExpect == 0 && rb.delivered == 0 {
		rb.nextExpect = nonce
	}

	// If this is the expected packet — deliver immediately
	if nonce == rb.nextExpect {
		rb.deliverPacket(data, nonce, pathID, now)
		rb.nextExpect++
		rb.inOrderCount++

		// Deliver any buffered consecutive packets
		rb.flushConsecutive()
		return
	}

	// If this packet is older than what we've delivered — discard (duplicate/late)
	if nonce < rb.nextExpect {
		return // already delivered or skipped
	}

	// Future packet — buffer it
	idx := nonce % maxBufferSize
	if rb.slots[idx] != nil && rb.slots[idx].Nonce == nonce {
		return // duplicate
	}

	pkt := &BufferedPacket{
		Data:    make([]byte, len(data)),
		Nonce:   nonce,
		PathID:  pathID,
		Arrival: now,
	}
	copy(pkt.Data, data)
	rb.slots[idx] = pkt
	rb.reorderedCount++

	// Set timeout for the gap — if expected packet doesn't arrive in time, skip it
	timeout := rb.timeoutForPath(pathID)
	pkt.timer = time.AfterFunc(timeout, func() {
		rb.onGapTimeout(nonce)
	})
}

// flushConsecutive delivers any buffered packets that are now consecutive.
func (rb *ReorderBuffer) flushConsecutive() {
	for {
		idx := rb.nextExpect % maxBufferSize
		pkt := rb.slots[idx]
		if pkt == nil || pkt.Nonce != rb.nextExpect {
			break
		}
		// Cancel any pending timeout
		if pkt.timer != nil {
			pkt.timer.Stop()
		}
		rb.deliverPacket(pkt.Data, pkt.Nonce, pkt.PathID, pkt.Arrival)
		rb.slots[idx] = nil
		rb.nextExpect++
	}
}

// onGapTimeout fires when we've waited long enough for a missing packet.
// Skip the gap and deliver what we have.
func (rb *ReorderBuffer) onGapTimeout(waitingForNonce uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Only act if we're still waiting for this nonce
	if rb.nextExpect > waitingForNonce {
		return // already delivered or skipped
	}

	// Skip to this nonce
	rb.gapCount++
	rb.nextExpect = waitingForNonce

	// Adapt window — increase because we had a gap
	rb.lastGapTime = time.Now()
	newWindow := time.Duration(float64(rb.maxWindow) * adaptIncreaseRate)
	if newWindow > time.Duration(maxWindowMs)*time.Millisecond {
		newWindow = time.Duration(maxWindowMs) * time.Millisecond
	}
	rb.maxWindow = newWindow

	// Flush consecutive from the new position
	rb.flushConsecutive()
}

// deliverPacket sends a packet to the output channel.
func (rb *ReorderBuffer) deliverPacket(data []byte, nonce uint64, pathID int, arrival time.Time) {
	rb.delivered = nonce
	select {
	case rb.output <- &BufferedPacket{
		Data:    data,
		Nonce:   nonce,
		PathID:  pathID,
		Arrival: arrival,
	}:
	default:
		// Output channel full — drop (shouldn't happen with properly sized channel)
	}
}

// timeoutForPath returns the gap timeout based on measured path latency.
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

// UpdatePathRTT updates the measured RTT for a path.
// Called with keepalive round-trip measurements.
func (rb *ReorderBuffer) UpdatePathRTT(pathID int, rtt time.Duration) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	ps, ok := rb.pathRTT[pathID]
	if !ok {
		ps = &PathStats{}
		rb.pathRTT[pathID] = ps
	}

	if ps.RTT == 0 {
		ps.RTT = rtt
		ps.RTTVar = rtt / 2
	} else {
		// Exponential moving average (TCP-style)
		ps.RTTVar = time.Duration(0.75*float64(ps.RTTVar) + 0.25*abs64(float64(ps.RTT-rtt)))
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
func (rb *ReorderBuffer) Stats() (inOrder, reordered, gaps uint64, windowMs int64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.inOrderCount, rb.reorderedCount, rb.gapCount, rb.maxWindow.Milliseconds()
}

func abs64(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

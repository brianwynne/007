/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

package bond

import (
	"sync"
	"time"
)

// JitterBuffer provides playout-deadline-aware packet buffering for
// real-time media transport. Every packet waits exactly bufferDepth
// before delivery, giving FEC and ARQ time to fill gaps.
//
// Unlike the reorder buffer (event-driven gap timeouts), the jitter
// buffer is clock-driven: a playout ticker fires every packetInterval
// and delivers whatever is in the current slot. Empty slots at playout
// time are genuine loss.
//
// Architecture:
//
//	Insert(data, dataSeq, source) → ring buffer slot
//	playoutLoop (goroutine) → deliverFunc(data) every packetInterval
//
// The buffer depth is derived from the latency budget:
//
//	bufferDepth = LatencyBudgetMs - (K-1) * PacketIntervalMs
//
// This guarantees FEC-recovered packets always arrive before playout.
type JitterBuffer struct {
	mu sync.Mutex

	// Ring buffer
	slots   [maxJitterSlots]jitterSlot
	bufSize int // usable slots = bufferDepth / packetInterval

	// Sequence tracking
	baseSeq     uint64 // dataSeq of next slot to play out
	writeHead   uint64 // highest dataSeq inserted + 1
	initialized bool

	// Timing
	bufferDepth    time.Duration
	packetInterval time.Duration
	nextPlayout    time.Time

	// Playout goroutine
	ticker  *time.Ticker
	stopCh  chan struct{}
	stopped bool

	// Delivery callback — called from playoutLoop goroutine
	deliverFunc func([]byte)

	// Early NACK — sequences detected as missing on Insert
	earlyNACK []uint64

	// Stats
	deliveredCount uint64
	lateCount      uint64
	duplicateCount uint64
	missCount      uint64 // genuine loss (slot empty at playout)
	fecFillCount   uint64 // slots filled by FEC recovery
	arqFillCount   uint64 // slots filled by ARQ retransmit
	jumpCount      uint64 // sequence jumps (sender reset)
}

type jitterSlot struct {
	data    []byte
	dataSeq uint64
	source  packetSource
	filled  bool
}

type packetSource uint8

const (
	sourceNone packetSource = iota
	sourceData
	sourceFEC
	sourceARQ
)

const maxJitterSlots = 512 // power of 2 for fast modulo

// JitterConfig holds jitter buffer configuration.
type JitterConfig struct {
	BufferDepth    time.Duration // how long to hold packets before playout
	PacketInterval time.Duration // expected interval between packets (e.g., 20ms)
	DeliverFunc    func([]byte)  // called at playout time with packet data
}

// NewJitterBuffer creates a new playout-deadline-aware jitter buffer.
func NewJitterBuffer(cfg JitterConfig) *JitterBuffer {
	bufSize := int(cfg.BufferDepth / cfg.PacketInterval)
	if bufSize < 1 {
		bufSize = 1
	}
	if bufSize > maxJitterSlots {
		bufSize = maxJitterSlots
	}

	return &JitterBuffer{
		bufSize:        bufSize,
		bufferDepth:    cfg.BufferDepth,
		packetInterval: cfg.PacketInterval,
		deliverFunc:    cfg.DeliverFunc,
		stopCh:         make(chan struct{}),
	}
}

// Insert places a packet into the jitter buffer.
// Called for data packets, FEC-recovered packets, and ARQ retransmits.
// Returns true if accepted, false if late or duplicate.
func (jb *JitterBuffer) Insert(data []byte, dataSeq uint64, source packetSource) bool {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	// First packet: initialize and start playout
	if !jb.initialized {
		jb.baseSeq = dataSeq
		jb.writeHead = dataSeq
		jb.nextPlayout = time.Now().Add(jb.bufferDepth)
		jb.initialized = true
		jb.ticker = time.NewTicker(jb.packetInterval)
		go jb.playoutLoop()
	}

	// Late: already played out
	if dataSeq < jb.baseSeq {
		jb.lateCount++
		return false
	}

	// Too far ahead: sequence jump
	if dataSeq >= jb.baseSeq+uint64(jb.bufSize) {
		jb.handleSequenceJump(dataSeq)
	}

	idx := dataSeq % maxJitterSlots
	slot := &jb.slots[idx]

	// Duplicate: slot already filled with this sequence
	if slot.filled && slot.dataSeq == dataSeq {
		jb.duplicateCount++
		return false
	}

	// Fill the slot — zero-copy, take ownership
	slot.data = data
	slot.dataSeq = dataSeq
	slot.source = source
	slot.filled = true

	switch source {
	case sourceFEC:
		jb.fecFillCount++
	case sourceARQ:
		jb.arqFillCount++
	}

	// Detect gaps for early NACK
	if dataSeq > jb.writeHead {
		for seq := jb.writeHead; seq < dataSeq; seq++ {
			gapIdx := seq % maxJitterSlots
			if !jb.slots[gapIdx].filled || jb.slots[gapIdx].dataSeq != seq {
				jb.earlyNACK = append(jb.earlyNACK, seq)
			}
		}
	}

	if dataSeq >= jb.writeHead {
		jb.writeHead = dataSeq + 1
	}

	return true
}

// playoutLoop delivers packets at fixed intervals.
func (jb *JitterBuffer) playoutLoop() {
	for {
		select {
		case <-jb.stopCh:
			return
		case <-jb.ticker.C:
			jb.playoutOne()
		}
	}
}

// playoutOne delivers the next packet or records a miss.
func (jb *JitterBuffer) playoutOne() {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if !jb.initialized {
		return
	}

	now := time.Now()
	if now.Before(jb.nextPlayout) {
		return
	}

	// Deliver all packets whose playout time has passed
	for !now.Before(jb.nextPlayout) {
		idx := jb.baseSeq % maxJitterSlots
		slot := &jb.slots[idx]

		if slot.filled && slot.dataSeq == jb.baseSeq {
			// Deliver
			if jb.deliverFunc != nil {
				jb.deliverFunc(slot.data)
			}
			jb.deliveredCount++
			slot.data = nil
			slot.filled = false
			slot.source = sourceNone
		} else {
			// Genuine loss
			jb.missCount++
		}

		jb.baseSeq++
		jb.nextPlayout = jb.nextPlayout.Add(jb.packetInterval)
	}
}

// handleSequenceJump resets the buffer when a large gap is detected.
func (jb *JitterBuffer) handleSequenceJump(newSeq uint64) {
	jb.jumpCount++
	// Position so newSeq is at the start of the buffer
	jb.baseSeq = newSeq
	jb.writeHead = newSeq
	jb.nextPlayout = time.Now().Add(jb.bufferDepth)
	// Clear all slots
	for i := range jb.slots {
		jb.slots[i] = jitterSlot{}
	}
}

// DrainEarlyNACK returns and clears sequences detected as missing.
func (jb *JitterBuffer) DrainEarlyNACK() []uint64 {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	if len(jb.earlyNACK) == 0 {
		return nil
	}
	nonces := jb.earlyNACK
	jb.earlyNACK = nil
	return nonces
}

// PlayoutDeadline returns the playout time for a given dataSeq.
// Used by ARQ to check if a retransmit can arrive in time.
func (jb *JitterBuffer) PlayoutDeadline(dataSeq uint64) time.Time {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	if !jb.initialized || dataSeq < jb.baseSeq {
		return time.Time{} // already played
	}
	offset := dataSeq - jb.baseSeq
	return jb.nextPlayout.Add(time.Duration(offset) * jb.packetInterval)
}

// Stop shuts down the playout goroutine.
func (jb *JitterBuffer) Stop() {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	if jb.stopped {
		return
	}
	jb.stopped = true
	close(jb.stopCh)
	if jb.ticker != nil {
		jb.ticker.Stop()
	}
}

// Stats returns jitter buffer statistics.
type JitterStats struct {
	Delivered  uint64
	Late       uint64
	Duplicates uint64
	Misses     uint64 // genuine loss
	FECFills   uint64
	ARQFills   uint64
	Jumps      uint64
	DepthMs    int64
	BufferSize int
}

func (jb *JitterBuffer) Stats() JitterStats {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	return JitterStats{
		Delivered:  jb.deliveredCount,
		Late:       jb.lateCount,
		Duplicates: jb.duplicateCount,
		Misses:     jb.missCount,
		FECFills:   jb.fecFillCount,
		ARQFills:   jb.arqFillCount,
		Jumps:      jb.jumpCount,
		DepthMs:    jb.bufferDepth.Milliseconds(),
		BufferSize: jb.bufSize,
	}
}

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
// real-time media transport. Each packet is held for exactly bufferDepth
// from its insertion time, then delivered. This works for any packet rate.
//
// Each packet gets its own playout deadline: insertTime + bufferDepth.
// A ticker checks every tickInterval if the oldest packet's deadline
// has passed. If yes, deliver and advance. No assumption about packet rate.
//
// FEC-recovered and ARQ-retransmitted packets can fill gaps as long as
// they arrive before the gap's playout deadline.
type JitterBuffer struct {
	mu sync.Mutex

	// Ring buffer
	slots   [maxJitterSlots]jitterSlot
	bufSize int

	// Sequence tracking
	baseSeq     uint64 // next sequence to play out
	writeHead   uint64 // highest dataSeq inserted + 1
	initialized bool

	// Timing
	bufferDepth time.Duration
	tickInterval time.Duration // how often to check for playable packets (1ms)

	// Playout goroutine
	ticker  *time.Ticker
	stopCh  chan struct{}
	stopped bool

	// Delivery callback
	deliverFunc func([]byte)

	// Early NACK
	earlyNACK []uint64

	// Stats
	deliveredCount uint64
	lateCount      uint64
	duplicateCount uint64
	missCount      uint64
	fecFillCount   uint64
	arqFillCount   uint64
	jumpCount      uint64
}

type jitterSlot struct {
	data     []byte
	dataSeq  uint64
	source   packetSource
	filled   bool
	deadline time.Time // when this slot should be played out
}

type packetSource uint8

const (
	sourceNone packetSource = iota
	sourceData
	sourceFEC
	sourceARQ
)

const maxJitterSlots = 512

// JitterConfig holds jitter buffer configuration.
type JitterConfig struct {
	BufferDepth    time.Duration // how long to hold each packet before playout
	PacketInterval time.Duration // expected interval (used for buffer sizing only)
	DeliverFunc    func([]byte)  // called at playout time with packet data
}

// NewJitterBuffer creates a new jitter buffer.
func NewJitterBuffer(cfg JitterConfig) *JitterBuffer {
	bufSize := int(cfg.BufferDepth / cfg.PacketInterval)
	if bufSize < 1 {
		bufSize = 1
	}
	if bufSize > maxJitterSlots {
		bufSize = maxJitterSlots
	}

	return &JitterBuffer{
		bufSize:      bufSize,
		bufferDepth:  cfg.BufferDepth,
		tickInterval: time.Millisecond, // check every 1ms for playable packets
		deliverFunc:  cfg.DeliverFunc,
		stopCh:       make(chan struct{}),
	}
}

// Insert places a packet into the jitter buffer.
// The packet will be delivered after bufferDepth from now.
// Returns true if accepted, false if late or duplicate.
func (jb *JitterBuffer) Insert(data []byte, dataSeq uint64, source packetSource) bool {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	now := time.Now()

	// First packet: initialize and start playout
	if !jb.initialized {
		jb.baseSeq = dataSeq
		jb.writeHead = dataSeq
		jb.initialized = true
		jb.ticker = time.NewTicker(jb.tickInterval)
		go jb.playoutLoop()
	}

	// Late: already played out
	if dataSeq < jb.baseSeq {
		jb.lateCount++
		return false
	}

	// Too far ahead: sequence jump
	if dataSeq >= jb.baseSeq+uint64(jb.bufSize) {
		jb.handleSequenceJump(dataSeq, now)
	}

	idx := dataSeq % maxJitterSlots
	slot := &jb.slots[idx]

	// Duplicate
	if slot.filled && slot.dataSeq == dataSeq {
		jb.duplicateCount++
		return false
	}

	// Fill the slot with its own playout deadline
	slot.data = data
	slot.dataSeq = dataSeq
	slot.source = source
	slot.filled = true
	slot.deadline = now.Add(jb.bufferDepth)

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

// playoutLoop checks for playable packets every tickInterval.
func (jb *JitterBuffer) playoutLoop() {
	for {
		select {
		case <-jb.stopCh:
			return
		case <-jb.ticker.C:
			jb.playoutReady()
		}
	}
}

// playoutReady delivers all packets whose deadline has passed.
func (jb *JitterBuffer) playoutReady() {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if !jb.initialized {
		return
	}

	now := time.Now()

	for {
		// Nothing to play
		if jb.baseSeq >= jb.writeHead {
			break
		}

		idx := jb.baseSeq % maxJitterSlots
		slot := &jb.slots[idx]

		if slot.filled && slot.dataSeq == jb.baseSeq {
			// Packet present — check if deadline passed
			if now.Before(slot.deadline) {
				break // not time yet
			}
			// Deliver
			if jb.deliverFunc != nil {
				jb.deliverFunc(slot.data)
			}
			jb.deliveredCount++
			slot.data = nil
			slot.filled = false
			slot.source = sourceNone
			jb.baseSeq++
		} else {
			// Slot empty — has enough time passed that we should skip it?
			// Check if the NEXT filled slot's deadline has passed.
			// If so, this gap is genuine loss — skip it.
			nextFilled := jb.findNextFilled()
			if nextFilled == 0 {
				break // no data ahead, wait
			}
			nextIdx := nextFilled % maxJitterSlots
			nextSlot := &jb.slots[nextIdx]
			if now.Before(nextSlot.deadline) {
				break // next packet isn't ready yet, wait for gap to fill
			}
			// Next packet is past deadline — this gap is genuine loss
			jb.missCount++
			jb.baseSeq++
		}
	}
}

// findNextFilled returns the dataSeq of the next filled slot after baseSeq.
// Returns 0 if none found.
func (jb *JitterBuffer) findNextFilled() uint64 {
	for offset := uint64(1); offset < uint64(jb.bufSize); offset++ {
		seq := jb.baseSeq + offset
		idx := seq % maxJitterSlots
		if jb.slots[idx].filled && jb.slots[idx].dataSeq == seq {
			return seq
		}
	}
	return 0
}

// handleSequenceJump resets the buffer for a large gap.
func (jb *JitterBuffer) handleSequenceJump(newSeq uint64, now time.Time) {
	jb.jumpCount++
	jb.baseSeq = newSeq
	jb.writeHead = newSeq
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
func (jb *JitterBuffer) PlayoutDeadline(dataSeq uint64) time.Time {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	if !jb.initialized || dataSeq < jb.baseSeq {
		return time.Time{}
	}
	idx := dataSeq % maxJitterSlots
	slot := &jb.slots[idx]
	if slot.filled && slot.dataSeq == dataSeq {
		return slot.deadline
	}
	// Not yet inserted — estimate based on buffer depth from now
	return time.Now().Add(jb.bufferDepth)
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

// JitterStats holds jitter buffer statistics.
type JitterStats struct {
	Delivered  uint64
	Late       uint64
	Duplicates uint64
	Misses     uint64
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

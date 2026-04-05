/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

package bond

import (
	"encoding/binary"
	"sync"
	"time"
)

// ARQ implements Automatic Repeat Request — NACK-based retransmission
// for packets that FEC cannot recover.
//
// Design:
//   - Sender maintains a ring buffer of recent cleartext packets + nonces
//   - Receiver detects unrecoverable gaps (reorder buffer timeout + FEC failure)
//   - Receiver sends NACK (list of missing nonces) as a control packet
//   - Sender retransmits original IP packets from the buffer
//   - Retransmitted packets re-enter the send path with new nonce/FEC
//
// Control packets use reserved FEC header values:
//
//	[BlockID=0xFFFF][Type (8)][K=0][M=0][payload...]
//	Type 1 = NACK: [count (uint16)][nonce1 (uint64)]...
//
// Rate limiting: max 1 NACK per 10ms to avoid flooding.

const (
	controlBlockID      = 0xFFFF // reserved FEC blockID for control packets
	controlTypeNACK     = 1
	controlTypeRetransmit = 4

	retransmitBufSize = 512   // packets in retransmit ring buffer
	maxNACKNonces     = 32    // max nonces per NACK packet
	nackRateLimit     = 10 * time.Millisecond // min time between NACKs
)

// SendFunc is a callback for injecting packets into the WireGuard send path.
// The bond manager calls this to send NACKs and retransmissions.
// The data is a cleartext IP packet that enters the normal send pipeline.
type SendFunc func(data []byte)

// retransmitBuffer stores recent cleartext packets for retransmission.
// Uses a ring buffer for bounded memory + a map for O(1) lookup by nonce.
type retransmitBuffer struct {
	mu      sync.Mutex
	entries [retransmitBufSize]retransmitEntry
	index   map[uint64]int // nonce → ring buffer position
	head    int            // next write position
}

type retransmitEntry struct {
	data  []byte
	nonce uint64
}

// Store saves a cleartext packet and its nonce for potential retransmission.
func (rb *retransmitBuffer) Store(data []byte, nonce uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.index == nil {
		rb.index = make(map[uint64]int, retransmitBufSize)
	}

	// Evict old entry at this ring position
	old := rb.entries[rb.head]
	if old.data != nil {
		delete(rb.index, old.nonce)
	}

	cp := make([]byte, len(data))
	copy(cp, data)
	rb.entries[rb.head] = retransmitEntry{data: cp, nonce: nonce}
	rb.index[nonce] = rb.head
	rb.head = (rb.head + 1) % retransmitBufSize
}

// Lookup finds a packet by nonce in O(1). Returns nil if not found or expired.
func (rb *retransmitBuffer) Lookup(nonce uint64) []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.index == nil {
		return nil
	}
	pos, ok := rb.index[nonce]
	if !ok {
		return nil
	}
	entry := rb.entries[pos]
	if entry.nonce != nonce {
		return nil // stale index entry
	}
	cp := make([]byte, len(entry.data))
	copy(cp, entry.data)
	return cp
}

// nackTracker generates NACKs for unrecoverable gaps on the receive side.
type nackTracker struct {
	mu        sync.Mutex
	pending   []uint64      // nonces that need NACKing
	lastNACK  time.Time     // rate limiting
	rateLimit time.Duration // min time between NACKs
	maxNonces int           // max nonces per NACK packet
}

// AddMissing records a nonce that FEC couldn't recover.
func (nt *nackTracker) AddMissing(nonce uint64) {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	// Deduplicate
	for _, n := range nt.pending {
		if n == nonce {
			return
		}
	}
	nt.pending = append(nt.pending, nonce)

	// Cap pending list
	if len(nt.pending) > maxNACKNonces*4 {
		nt.pending = nt.pending[len(nt.pending)-maxNACKNonces*4:]
	}
}

// GenerateNACK returns a NACK control packet if there are pending
// missing nonces and rate limiting allows. Returns nil otherwise.
func (nt *nackTracker) GenerateNACK() []byte {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	if len(nt.pending) == 0 {
		return nil
	}

	now := time.Now()
	rl := nt.rateLimit
	if rl == 0 {
		rl = nackRateLimit
	}
	if now.Sub(nt.lastNACK) < rl {
		return nil
	}

	// Take up to maxNonces
	maxN := nt.maxNonces
	if maxN == 0 {
		maxN = maxNACKNonces
	}
	count := len(nt.pending)
	if count > maxN {
		count = maxN
	}
	nonces := nt.pending[:count]
	nt.pending = nt.pending[count:]
	nt.lastNACK = now

	return buildNACKPacket(nonces)
}

// buildNACKPacket creates a control packet with missing nonces.
// Format: [FEC header (5)][count (2)][nonce1 (8)][nonce2 (8)]...
func buildNACKPacket(nonces []uint64) []byte {
	pkt := make([]byte, FECHeaderSize+2+len(nonces)*8)
	binary.BigEndian.PutUint16(pkt[0:2], controlBlockID)
	pkt[2] = controlTypeNACK
	pkt[3] = 0 // K=0
	pkt[4] = 0 // M=0
	binary.BigEndian.PutUint16(pkt[FECHeaderSize:FECHeaderSize+2], uint16(len(nonces)))
	for i, n := range nonces {
		offset := FECHeaderSize + 2 + i*8
		binary.BigEndian.PutUint64(pkt[offset:offset+8], n)
	}
	return pkt
}

// parseNACKPacket extracts missing nonces from a NACK control packet.
func parseNACKPacket(pkt []byte) []uint64 {
	if len(pkt) < FECHeaderSize+2 {
		return nil
	}
	count := int(binary.BigEndian.Uint16(pkt[FECHeaderSize : FECHeaderSize+2]))
	if count > maxNACKNonces {
		count = maxNACKNonces // cap to prevent over-allocation
	}
	if len(pkt) < FECHeaderSize+2+count*8 {
		return nil
	}
	nonces := make([]uint64, count)
	for i := range nonces {
		offset := FECHeaderSize + 2 + i*8
		nonces[i] = binary.BigEndian.Uint64(pkt[offset : offset+8])
	}
	return nonces
}

// buildRetransmitPacket creates a control packet carrying a retransmitted
// IP payload with its original dataSeq, so the receiver can insert it
// at the correct position in the reorder buffer.
// Format: [blockID=0xFFFF][type=4][k=0][m=0][dataSeq (8)][IP payload]
func buildRetransmitPacket(dataSeq uint64, payload []byte) []byte {
	pkt := make([]byte, FECHeaderSize+8+len(payload))
	binary.BigEndian.PutUint16(pkt[0:2], controlBlockID)
	pkt[2] = controlTypeRetransmit
	pkt[3] = 0
	pkt[4] = 0
	binary.BigEndian.PutUint64(pkt[FECHeaderSize:FECHeaderSize+8], dataSeq)
	copy(pkt[FECHeaderSize+8:], payload)
	return pkt
}

// parseRetransmitPacket extracts dataSeq and IP payload from a RETRANSMIT packet.
func parseRetransmitPacket(pkt []byte) (dataSeq uint64, payload []byte) {
	if len(pkt) < FECHeaderSize+8 {
		return 0, nil
	}
	dataSeq = binary.BigEndian.Uint64(pkt[FECHeaderSize : FECHeaderSize+8])
	if len(pkt) > FECHeaderSize+8 {
		payload = make([]byte, len(pkt)-FECHeaderSize-8)
		copy(payload, pkt[FECHeaderSize+8:])
	}
	return dataSeq, payload
}

// isControlPacket checks if a packet is a bond control message (not data).
func isControlPacket(pkt []byte) bool {
	if len(pkt) < FECHeaderSize {
		return false
	}
	return binary.BigEndian.Uint16(pkt[0:2]) == controlBlockID
}

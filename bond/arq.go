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
	controlBlockID = 0xFFFF // reserved FEC blockID for control packets
	controlTypeNACK = 1

	retransmitBufSize = 512   // packets in retransmit ring buffer
	maxNACKNonces     = 32    // max nonces per NACK packet
	nackRateLimit     = 10 * time.Millisecond // min time between NACKs
)

// SendFunc is a callback for injecting packets into the WireGuard send path.
// The bond manager calls this to send NACKs and retransmissions.
// The data is a cleartext IP packet that enters the normal send pipeline.
type SendFunc func(data []byte)

// retransmitBuffer stores recent cleartext packets for retransmission.
type retransmitBuffer struct {
	mu      sync.Mutex
	packets [retransmitBufSize]retransmitEntry
	head    int // next write position
}

type retransmitEntry struct {
	data  []byte
	nonce uint64
	valid bool
}

// Store saves a cleartext packet and its nonce for potential retransmission.
func (rb *retransmitBuffer) Store(data []byte, nonce uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	cp := make([]byte, len(data))
	copy(cp, data)
	rb.packets[rb.head] = retransmitEntry{
		data:  cp,
		nonce: nonce,
		valid: true,
	}
	rb.head = (rb.head + 1) % retransmitBufSize
}

// Lookup finds a packet by nonce. Returns nil if not found or expired.
func (rb *retransmitBuffer) Lookup(nonce uint64) []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for i := range rb.packets {
		if rb.packets[i].valid && rb.packets[i].nonce == nonce {
			// Return a copy
			cp := make([]byte, len(rb.packets[i].data))
			copy(cp, rb.packets[i].data)
			return cp
		}
	}
	return nil
}

// nackTracker generates NACKs for unrecoverable gaps on the receive side.
type nackTracker struct {
	mu       sync.Mutex
	pending  []uint64  // nonces that need NACKing
	lastNACK time.Time // rate limiting
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
	if now.Sub(nt.lastNACK) < nackRateLimit {
		return nil
	}

	// Take up to maxNACKNonces
	count := len(nt.pending)
	if count > maxNACKNonces {
		count = maxNACKNonces
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

// isControlPacket checks if a packet is a bond control message (not data).
func isControlPacket(pkt []byte) bool {
	if len(pkt) < FECHeaderSize {
		return false
	}
	return binary.BigEndian.Uint16(pkt[0:2]) == controlBlockID
}

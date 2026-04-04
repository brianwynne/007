/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

// Package bond provides multi-path network bonding with FEC and packet reordering.
//
// Bond integrates with wireguard-go's send/receive pipeline to provide:
//   - Multi-path sending: every packet sent on all configured endpoints
//   - FEC: adaptive Reed-Solomon forward error correction
//   - Reordering: adaptive buffer that delivers packets in nonce order
//
// Integration points with wireguard-go:
//
// SEND PATH (device/send.go):
//
//	RoutineReadFromTUN → StagePackets → SendStagedPackets
//	  → nonce assigned (elem.nonce)
//	  → [FEC ENCODE HERE] ← bond intercepts after nonce assignment
//	  → encryption (RoutineEncryption)
//	  → RoutineSequentialSender → peer.SendBuffers (sends to ALL endpoints)
//
// RECEIVE PATH (device/receive.go):
//
//	RoutineReceiveIncoming → decryption (RoutineDecryption)
//	  → RoutineSequentialReceiver
//	  → replay filter validates elem.counter
//	  → [FEC DECODE + REORDER HERE] ← bond intercepts before TUN write
//	  → device.tun.device.Write()
//
// FEC and reorder state is per-peer: each peer gets its own encoder, decoder,
// and reorder buffer so packets from different peers are never mixed.
//
// The bond manager does NOT modify the encryption or handshake — those
// remain standard WireGuard. Bond operates on cleartext packets (send side)
// and decrypted packets (receive side).
package bond

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Config holds bond configuration.
type Config struct {
	// FEC
	FECEnabled  bool // enable forward error correction
	FECAdaptive bool // dynamically adjust FEC ratio

	// Reorder
	ReorderEnabled bool // enable reorder buffer

	// General
	Enabled bool // master enable for bonding features
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		FECEnabled:     true,
		FECAdaptive:    true,
		ReorderEnabled: true,
	}
}

// peerState holds per-peer FEC, reorder, and ARQ state.
type peerState struct {
	encoder    *FECEncoder
	decoder    *FECDecoder
	reorderBuf *ReorderBuffer
	retransmit *retransmitBuffer // sender side: recent packets for retransmission
	nackTrack  *nackTracker      // receiver side: tracks unrecoverable gaps
	sendFunc   func(data []byte)  // callback to inject packets into send path
}

// Manager coordinates FEC encoding/decoding and packet reordering.
// It sits between wireguard-go's TUN interface and crypto layer.
type Manager struct {
	mu     sync.RWMutex
	config Config

	// Per-peer state
	peers   map[uint32]*peerState
	peersMu sync.Mutex

	// Stats
	txPackets atomic.Uint64
	rxPackets atomic.Uint64

	// Lifecycle
	running atomic.Bool
	stopCh  chan struct{}

	// Logger
	logger *log.Logger
}

// NewManager creates a new bond manager with the given configuration.
func NewManager(cfg Config, logger *log.Logger) (*Manager, error) {
	m := &Manager{
		config: cfg,
		peers:  make(map[uint32]*peerState),
		stopCh: make(chan struct{}),
		logger: logger,
	}

	return m, nil
}

// getPeerState returns the state for a peer, creating it if needed.
func (m *Manager) getPeerState(peerID uint32) *peerState {
	m.peersMu.Lock()
	defer m.peersMu.Unlock()

	ps, exists := m.peers[peerID]
	if !exists {
		ps = &peerState{}
		if m.config.FECEnabled {
			enc, err := NewFECEncoder()
			if err != nil {
				if m.logger != nil {
					m.logger.Printf("007 Bond: failed to create FEC encoder for peer %d: %v", peerID, err)
				}
			} else {
				ps.encoder = enc
				ps.decoder = NewFECDecoder()
			}
		}
		if m.config.ReorderEnabled {
			ps.reorderBuf = NewReorderBuffer()
		}
		ps.retransmit = &retransmitBuffer{}
		ps.nackTrack = &nackTracker{}
		m.peers[peerID] = ps
	}
	return ps
}

// SetPeerSendFunc provides a callback for injecting packets into the
// WireGuard send path for a specific peer. Used by ARQ for NACKs and
// retransmissions. Must be called after the peer is created.
func (m *Manager) SetPeerSendFunc(peerID uint32, fn func(data []byte)) {
	ps := m.getPeerState(peerID)
	m.peersMu.Lock()
	ps.sendFunc = fn
	m.peersMu.Unlock()
}

// Start begins background goroutines for adaptation.
func (m *Manager) Start() {
	if m.running.Swap(true) {
		return // already running
	}

	// Adaptive FEC ratio goroutine
	if m.config.FECEnabled && m.config.FECAdaptive {
		go m.fecAdaptLoop()
	}

	// Adaptive reorder window + periodic flush goroutine
	if m.config.ReorderEnabled {
		go m.reorderLoop()
	}

	if m.logger != nil {
		m.logger.Println("007 Bond: manager started")
	}
}

// Stop shuts down background goroutines.
func (m *Manager) Stop() {
	if !m.running.Swap(false) {
		return
	}
	close(m.stopCh)
	if m.logger != nil {
		m.logger.Println("007 Bond: manager stopped")
	}
}

// ProcessOutbound handles a packet on the send path.
// Called after nonce assignment, before encryption.
//
// Returns the data packet (with FEC header) plus any parity packets.
// Each returned packet is independent and must be encrypted with its own nonce.
//
// If FEC is disabled, returns just the original packet unchanged.
func (m *Manager) ProcessOutbound(peerID uint32, packet []byte, nonce uint64) [][]byte {
	m.txPackets.Add(1)

	if !m.config.FECEnabled {
		return [][]byte{packet}
	}

	ps := m.getPeerState(peerID)

	// Store in retransmit buffer (before FEC encoding, keyed by nonce)
	ps.retransmit.Store(packet, nonce)

	if ps.encoder == nil {
		return [][]byte{packet}
	}

	// FEC encode — prepends header + nonce, generates parity when block is full
	encodedData, parityPackets := ps.encoder.Encode(packet, nonce)

	result := make([][]byte, 0, 1+len(parityPackets))
	result = append(result, encodedData)
	result = append(result, parityPackets...)
	return result
}

// ProcessInbound handles a packet on the receive path.
// Called after decryption and replay filter validation, before TUN write.
//
// Pipeline: FEC decode → reorder data packets → deliver recovered immediately.
// Returns IP packets ready for TUN delivery, or nil if buffered.
func (m *Manager) ProcessInbound(peerID uint32, packet []byte, nonce uint64, pathID int) [][]byte {
	m.rxPackets.Add(1)

	ps := m.getPeerState(peerID)

	// Check for control packets (NACK, etc.) — these are NOT data
	if isControlPacket(packet) {
		m.handleControl(ps, packet)
		return nil
	}

	// FEC decode
	if m.config.FECEnabled && ps.decoder != nil && len(packet) > FECHeaderSize {
		data, recovered := ps.decoder.Decode(packet)

		var result [][]byte

		// Data packet → reorder using its embedded nonce
		if data != nil {
			if m.config.ReorderEnabled && ps.reorderBuf != nil {
				result = append(result, ps.reorderBuf.Insert(data.Data, data.Nonce, pathID)...)
			} else {
				result = append(result, data.Data)
			}
		}

		// Recovered packets → also reorder (nonce recovered from FEC payload)
		for _, rec := range recovered {
			if m.config.ReorderEnabled && ps.reorderBuf != nil {
				result = append(result, ps.reorderBuf.Insert(rec.Data, rec.Nonce, pathID)...)
			} else {
				result = append(result, rec.Data)
			}
		}

		// Check for skipped nonces (FEC couldn't recover) → queue for NACK
		if ps.reorderBuf != nil {
			skipped := ps.reorderBuf.DrainSkippedNonces()
			for _, n := range skipped {
				ps.nackTrack.AddMissing(n)
			}
			if len(skipped) > 0 {
				m.triggerNACK(ps)
			}
		}

		return result
	}

	// No FEC — just reorder the raw packet
	if m.config.ReorderEnabled && ps.reorderBuf != nil {
		return ps.reorderBuf.Insert(packet, nonce, pathID)
	}

	return [][]byte{packet}
}

// handleControl processes a bond control packet (NACK, etc.).
func (m *Manager) handleControl(ps *peerState, packet []byte) {
	if len(packet) < FECHeaderSize {
		return
	}
	controlType := packet[2]

	switch controlType {
	case controlTypeNACK:
		// Sender received a NACK — retransmit requested packets
		nonces := parseNACKPacket(packet)
		if ps.sendFunc == nil {
			return
		}
		for _, nonce := range nonces {
			data := ps.retransmit.Lookup(nonce)
			if data != nil {
				ps.sendFunc(data) // re-inject into send path
			}
		}
		if m.logger != nil && len(nonces) > 0 {
			m.logger.Printf("007 Bond: retransmitted %d packets on NACK", len(nonces))
		}
	}
}

// triggerNACK generates and sends a NACK for unrecoverable gaps.
func (m *Manager) triggerNACK(ps *peerState) {
	nack := ps.nackTrack.GenerateNACK()
	if nack == nil || ps.sendFunc == nil {
		return
	}
	// Send NACK as a control packet through the WireGuard tunnel
	ps.sendFunc(nack)
}

// reorderLoop runs periodic flush and window adaptation for all peers.
func (m *Manager) reorderLoop() {
	flushTicker := time.NewTicker(10 * time.Millisecond)
	adaptTicker := time.NewTicker(1 * time.Second)
	defer flushTicker.Stop()
	defer adaptTicker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-flushTicker.C:
			// Flush timed-out gaps for all peers.
			// Returned packets are discarded — this just advances nextExpect
			// to prevent stale state. During active traffic, gap timeouts are
			// resolved synchronously inside Insert() before this fires.
			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.reorderBuf != nil {
					ps.reorderBuf.Flush()
				}
			}
			m.peersMu.Unlock()
		case <-adaptTicker.C:
			// Adapt window for all peers
			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.reorderBuf != nil {
					ps.reorderBuf.AdaptWindow()
				}
			}
			m.peersMu.Unlock()
		}
	}
}

// fecAdaptLoop periodically adjusts the FEC ratio based on measured loss.
func (m *Manager) fecAdaptLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastTx, lastRx uint64

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			tx := m.txPackets.Load()
			rx := m.rxPackets.Load()

			deltaTx := tx - lastTx
			deltaRx := rx - lastRx
			lastTx = tx
			lastRx = rx

			if deltaTx == 0 {
				continue
			}

			lossRate := 0.0
			if deltaTx > deltaRx {
				lossRate = float64(deltaTx-deltaRx) / float64(deltaTx)
			}

			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.encoder != nil {
					if err := ps.encoder.AdaptRate(lossRate); err != nil && m.logger != nil {
						m.logger.Printf("007 Bond: FEC adapt error: %v", err)
					}
				}
			}
			m.peersMu.Unlock()
		}
	}
}

// Stats returns current bond statistics.
type Stats struct {
	TxPackets       uint64
	RxPackets       uint64
	FECRecovered    uint64
	FECFailed       uint64
	ReorderInOrder  uint64
	ReorderReordered uint64
	ReorderGaps     uint64
	ReorderWindowMs int64
}

// GetStats returns current statistics aggregated across all peers.
func (m *Manager) GetStats() Stats {
	s := Stats{
		TxPackets: m.txPackets.Load(),
		RxPackets: m.rxPackets.Load(),
	}

	m.peersMu.Lock()
	for _, ps := range m.peers {
		if ps.decoder != nil {
			recovered, failed := ps.decoder.Stats()
			s.FECRecovered += recovered
			s.FECFailed += failed
		}
		if ps.reorderBuf != nil {
			inOrder, reordered, gaps, windowMs := ps.reorderBuf.Stats()
			s.ReorderInOrder += inOrder
			s.ReorderReordered += reordered
			s.ReorderGaps += gaps
			if windowMs > s.ReorderWindowMs {
				s.ReorderWindowMs = windowMs
			}
		}
	}
	m.peersMu.Unlock()

	return s
}

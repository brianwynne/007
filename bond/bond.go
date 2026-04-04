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
//	  → [REORDER + FEC DECODE HERE] ← bond intercepts before TUN write
//	  → device.tun.device.Write()
//
// FEC state is per-peer: each peer gets its own encoder/decoder so packets
// from different peers are never mixed in the same FEC block. The peerID
// parameter in ProcessOutbound/ProcessInbound identifies the peer.
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
	ReorderEnabled  bool // enable reorder buffer
	ReorderWindowMs int  // max reorder window (milliseconds)
	ReorderMinMs    int  // minimum window (milliseconds)

	// General
	Enabled bool // master enable for bonding features
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		FECEnabled:      true,
		FECAdaptive:     true,
		ReorderEnabled:  true,
		ReorderWindowMs: 80,
		ReorderMinMs:    20,
	}
}

// peerState holds per-peer FEC encoder/decoder state.
type peerState struct {
	encoder *FECEncoder
	decoder *FECDecoder
}

// Manager coordinates FEC encoding/decoding and packet reordering.
// It sits between wireguard-go's TUN interface and crypto layer.
type Manager struct {
	mu     sync.RWMutex
	config Config

	// Per-peer FEC state
	peers   map[uint32]*peerState
	peersMu sync.Mutex

	// Reorder (global for now — will be per-peer when wired in)
	reorderBuf *ReorderBuffer

	// Stats
	txPackets    atomic.Uint64
	rxPackets    atomic.Uint64
	fecRecovered atomic.Uint64
	fecFailed    atomic.Uint64
	reorderGaps  atomic.Uint64

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

	if cfg.ReorderEnabled {
		m.reorderBuf = NewReorderBuffer(256)
	}

	return m, nil
}

// getPeerState returns the FEC state for a peer, creating it if needed.
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
				return ps
			}
			ps.encoder = enc
			ps.decoder = NewFECDecoder()
		}
		m.peers[peerID] = ps
	}
	return ps
}

// Start begins background goroutines for adaptation and stats.
func (m *Manager) Start() {
	if m.running.Swap(true) {
		return // already running
	}

	// Adaptive window adjustment goroutine
	if m.reorderBuf != nil {
		go m.reorderAdaptLoop()
	}

	// Adaptive FEC ratio goroutine
	if m.config.FECEnabled && m.config.FECAdaptive {
		go m.fecAdaptLoop()
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
	if ps.encoder == nil {
		return [][]byte{packet}
	}

	// FEC encode — prepends 5-byte header to data, generates parity when block is full
	encodedData, parityPackets := ps.encoder.Encode(packet)

	result := make([][]byte, 0, 1+len(parityPackets))
	result = append(result, encodedData)
	result = append(result, parityPackets...)
	return result
}

// ProcessInbound handles a packet on the receive path.
// Called after decryption and replay filter validation, before TUN write.
//
// Returns IP packets ready for TUN delivery (FEC header stripped), or nil
// if the packet is parity-only with no recovery possible yet.
func (m *Manager) ProcessInbound(peerID uint32, packet []byte, nonce uint64, pathID int) [][]byte {
	m.rxPackets.Add(1)

	// If neither FEC nor reorder is enabled, pass through
	if !m.config.FECEnabled && !m.config.ReorderEnabled {
		return [][]byte{packet}
	}

	// FEC decode — returns clean IP packets (header stripped, possibly recovered)
	if m.config.FECEnabled && len(packet) > FECHeaderSize {
		ps := m.getPeerState(peerID)
		if ps.decoder != nil {
			return ps.decoder.Decode(packet)
		}
	}

	return [][]byte{packet}
}

// reorderAdaptLoop periodically adjusts the reorder buffer window.
func (m *Manager) reorderAdaptLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reorderBuf.AdaptWindow()
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

			// Estimate loss rate from TX/RX difference
			lossRate := 0.0
			if deltaTx > deltaRx {
				lossRate = float64(deltaTx-deltaRx) / float64(deltaTx)
			}

			// Adapt all peer encoders
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
	ReorderGaps     uint64
	ReorderWindowMs int64
	InOrder         uint64
	Reordered       uint64
}

// GetStats returns current statistics.
func (m *Manager) GetStats() Stats {
	s := Stats{
		TxPackets: m.txPackets.Load(),
		RxPackets: m.rxPackets.Load(),
	}

	// Aggregate FEC stats from all peers
	m.peersMu.Lock()
	for _, ps := range m.peers {
		if ps.decoder != nil {
			recovered, failed := ps.decoder.Stats()
			s.FECRecovered += recovered
			s.FECFailed += failed
		}
	}
	m.peersMu.Unlock()

	if m.reorderBuf != nil {
		s.InOrder, s.Reordered, s.ReorderGaps, s.ReorderWindowMs = m.reorderBuf.Stats()
	}

	return s
}

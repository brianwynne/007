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
	FECEnabled bool // enable forward error correction
	FECAdaptive bool // dynamically adjust FEC ratio

	// Reorder
	ReorderEnabled  bool          // enable reorder buffer
	ReorderWindowMs int           // max reorder window (milliseconds)
	ReorderMinMs    int           // minimum window (milliseconds)

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

// Manager coordinates FEC encoding/decoding and packet reordering.
// It sits between wireguard-go's TUN interface and crypto layer.
type Manager struct {
	mu     sync.RWMutex
	config Config

	// FEC
	encoder *FECEncoder
	decoder *FECDecoder

	// Reorder
	reorderBuf *ReorderBuffer

	// Stats
	txPackets     atomic.Uint64
	rxPackets     atomic.Uint64
	fecRecovered  atomic.Uint64
	fecFailed     atomic.Uint64
	reorderGaps   atomic.Uint64

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
		stopCh: make(chan struct{}),
		logger: logger,
	}

	if cfg.FECEnabled {
		enc, err := NewFECEncoder()
		if err != nil {
			return nil, err
		}
		m.encoder = enc
		m.decoder = NewFECDecoder(256)
	}

	if cfg.ReorderEnabled {
		m.reorderBuf = NewReorderBuffer(256)
	}

	return m, nil
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
	if m.encoder != nil && m.config.FECAdaptive {
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
// Returns the original packet plus any FEC parity packets that should
// also be encrypted and sent. Each returned packet is independent and
// should be encrypted with its own nonce.
//
// If FEC is disabled, returns just the original packet unchanged.
func (m *Manager) ProcessOutbound(packet []byte, nonce uint64) [][]byte {
	m.txPackets.Add(1)

	if !m.config.FECEnabled || m.encoder == nil {
		return [][]byte{packet}
	}

	// FEC encode — may return additional parity packets
	parityPackets := m.encoder.Encode(packet)

	if len(parityPackets) == 0 {
		return [][]byte{packet}
	}

	// Return original + parity packets
	// Each parity packet needs its own nonce and encryption
	result := make([][]byte, 0, 1+len(parityPackets))
	result = append(result, packet)
	result = append(result, parityPackets...)
	return result
}

// ProcessInbound handles a packet on the receive path.
// Called after decryption and replay filter validation, before TUN write.
//
// The packet is inserted into the reorder buffer (if enabled) and/or
// FEC decoder. Returns packets ready for TUN delivery in-order, or nil
// if the packet is buffered waiting for earlier sequences.
//
// Parameters:
//   - packet: decrypted packet data
//   - nonce: WireGuard nonce (sequence number)
//   - pathID: which network path delivered this packet (for per-path stats)
func (m *Manager) ProcessInbound(packet []byte, nonce uint64, pathID int) [][]byte {
	m.rxPackets.Add(1)

	// If neither FEC nor reorder is enabled, pass through
	if !m.config.FECEnabled && !m.config.ReorderEnabled {
		return [][]byte{packet}
	}

	// FEC decode (if packet has FEC header)
	if m.config.FECEnabled && m.decoder != nil && len(packet) > FECHeaderSize {
		m.decoder.Decode(packet)
		// Recovered packets come via decoder output channel
		// For now, also pass the data portion through
		packet = packet[FECHeaderSize:]
	}

	// Reorder buffer
	if m.config.ReorderEnabled && m.reorderBuf != nil {
		m.reorderBuf.Insert(packet, nonce, pathID)

		// Collect all ready packets from the output channel
		var ready [][]byte
		for {
			select {
			case pkt := <-m.reorderBuf.Output():
				ready = append(ready, pkt.Data)
			default:
				return ready
			}
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
			// This is approximate — real loss measurement needs RTCP-like feedback
			lossRate := 0.0
			if deltaTx > deltaRx {
				lossRate = float64(deltaTx-deltaRx) / float64(deltaTx)
			}

			if err := m.encoder.AdaptRate(lossRate); err != nil && m.logger != nil {
				m.logger.Printf("007 Bond: FEC adapt error: %v", err)
			}
		}
	}
}

// Stats returns current bond statistics.
type Stats struct {
	TxPackets    uint64
	RxPackets    uint64
	FECRecovered uint64
	FECFailed    uint64
	ReorderGaps  uint64
	ReorderWindowMs int64
	InOrder      uint64
	Reordered    uint64
}

// GetStats returns current statistics.
func (m *Manager) GetStats() Stats {
	s := Stats{
		TxPackets: m.txPackets.Load(),
		RxPackets: m.rxPackets.Load(),
	}

	if m.decoder != nil {
		s.FECRecovered, s.FECFailed = m.decoder.Stats()
	}
	if m.reorderBuf != nil {
		s.InOrder, s.Reordered, s.ReorderGaps, s.ReorderWindowMs = m.reorderBuf.Stats()
	}

	return s
}

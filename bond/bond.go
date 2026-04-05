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

// SystemState represents the overall health of the bonding system.
type SystemState int32

const (
	SystemHealthy       SystemState = iota // all paths healthy, recovery working
	SystemDegraded                          // some paths impaired, recovery active
	SystemSeverelyDegraded                  // most paths impaired, recovery stressed
	SystemUnrecoverable                     // loss exceeding recovery capacity
)

func (s SystemState) String() string {
	switch s {
	case SystemHealthy:
		return "healthy"
	case SystemDegraded:
		return "degraded"
	case SystemSeverelyDegraded:
		return "severely_degraded"
	case SystemUnrecoverable:
		return "unrecoverable"
	default:
		return "unknown"
	}
}

// Config holds bond configuration. All operational parameters are
// configurable — no hardcoded constants per CLAUDE.md requirements.
type Config struct {
	// General
	Enabled        bool          // master enable for bonding features
	LatencyBudgetMs int          // max acceptable end-to-end latency (ms); 0 = unlimited

	// FEC
	FECEnabled      bool         // enable forward error correction
	FECAdaptive     bool         // dynamically adjust FEC ratio
	FECBlockTimeoutMs int        // max ms to wait for complete FEC block
	FECLossWindow   int          // packets in loss measurement sliding window
	FECAdaptIntervalMs int       // ms between FEC ratio adaptation
	FECLowK, FECLowM   int      // clean network FEC ratio
	FECMedK, FECMedM   int      // moderate loss FEC ratio
	FECHighK, FECHighM int      // high loss FEC ratio
	FECLowThreshold    float64  // loss rate threshold for low→med
	FECHighThreshold   float64  // loss rate threshold for med→high

	// Reorder
	ReorderEnabled    bool       // enable reorder buffer
	ReorderBufSize    int        // max packets in reorder buffer
	ReorderWindowMs   int        // default reorder window (ms)
	ReorderMinMs      int        // minimum reorder window (ms)
	ReorderMaxMs      int        // maximum reorder window (ms)
	ReorderFlushMs    int        // gap flush interval (ms)
	ReorderAdaptSec   int        // window adaptation interval (seconds)

	// ARQ
	ARQEnabled        bool       // enable NACK-based retransmission
	ARQBufSize        int        // retransmit ring buffer size (packets)
	ARQMaxNonces      int        // max nonces per NACK packet
	ARQRateLimitMs    int        // min ms between NACKs
	ARQDeadlineCheck  bool       // only NACK if retransmit can arrive in time

	// Path health
	ProbeIntervalMs    int       // ms between path probes
	PathStatsWindow    int       // packets in loss measurement window
	PacketIntervalMs   int       // expected packet interval for jitter calc (ms)
}

// DefaultConfig returns a sensible default configuration for broadcast audio.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		LatencyBudgetMs: 200, // 200ms end-to-end budget for broadcast

		FECEnabled:        true,
		FECAdaptive:       true,
		FECBlockTimeoutMs: 50,
		FECLossWindow:     200,
		FECAdaptIntervalMs: 500,
		FECLowK: 16, FECLowM: 2,
		FECMedK: 12, FECMedM: 4,
		FECHighK: 8, FECHighM: 6,
		FECLowThreshold:  0.01,
		FECHighThreshold: 0.05,

		ReorderEnabled:  true,
		ReorderBufSize:  64,
		ReorderWindowMs: 80,
		ReorderMinMs:    20,
		ReorderMaxMs:    200,
		ReorderFlushMs:  10,
		ReorderAdaptSec: 1,

		ARQEnabled:       true,
		ARQBufSize:       512,
		ARQMaxNonces:     32,
		ARQRateLimitMs:   10,
		ARQDeadlineCheck: true,

		ProbeIntervalMs:  1000,
		PathStatsWindow:  100,
		PacketIntervalMs: 20,
	}
}

// peerState holds per-peer FEC, reorder, ARQ, and path health state.
type peerState struct {
	encoder    *FECEncoder
	decoder    *FECDecoder
	reorderBuf *ReorderBuffer
	retransmit *retransmitBuffer // sender side: recent packets for retransmission
	nackTrack  *nackTracker      // receiver side: tracks unrecoverable gaps
	pathTrack  *pathTracker      // per-path health metrics
	sendFunc   func(data []byte) // callback to inject packets into send path
}

// Manager coordinates FEC encoding/decoding and packet reordering.
// It sits between wireguard-go's TUN interface and crypto layer.
type Manager struct {
	mu     sync.RWMutex
	config Config

	// Per-peer state
	peers   map[uint32]*peerState
	peersMu sync.Mutex

	// System state
	systemState atomic.Int32 // SystemState enum

	// Stats
	txPackets   atomic.Uint64
	rxPackets   atomic.Uint64
	dropPackets atomic.Uint64

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
			enc, err := NewFECEncoder(m.config)
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
			ps.reorderBuf = NewReorderBuffer(m.config)
		}
		ps.retransmit = &retransmitBuffer{}
		ps.nackTrack = &nackTracker{
			rateLimit: time.Duration(m.config.ARQRateLimitMs) * time.Millisecond,
			maxNonces: m.config.ARQMaxNonces,
		}
		ps.pathTrack = newPathTracker()
		m.peers[peerID] = ps
	}
	return ps
}

// RemovePeer cleans up all state for a peer. Called when a peer is removed.
func (m *Manager) RemovePeer(peerID uint32) {
	m.peersMu.Lock()
	defer m.peersMu.Unlock()
	delete(m.peers, peerID)
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

	// Path health probing goroutine
	go m.probeLoop()

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

	ps := m.getPeerState(peerID)

	// Always store in retransmit buffer for ARQ (regardless of FEC)
	ps.retransmit.Store(packet, nonce)

	if !m.config.FECEnabled || ps.encoder == nil {
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

	// Single timestamp for all operations on this packet
	now := time.Now()

	// Track per-path receive stats
	ps.pathTrack.RecordReceive(pathID)

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
				result = append(result, ps.reorderBuf.InsertAt(data.Data, data.Nonce, pathID, now)...)
			} else {
				result = append(result, data.Data)
			}
		}

		// Recovered packets → also reorder (nonce recovered from FEC payload)
		for _, rec := range recovered {
			if m.config.ReorderEnabled && ps.reorderBuf != nil {
				result = append(result, ps.reorderBuf.InsertAt(rec.Data, rec.Nonce, pathID, now)...)
			} else {
				result = append(result, rec.Data)
			}
		}

		// Check for skipped nonces (FEC couldn't recover) → queue for NACK
		if ps.reorderBuf != nil && m.config.ARQEnabled {
			skipped := ps.reorderBuf.DrainSkippedNonces()
			for _, n := range skipped {
				// Only NACK if retransmit can arrive within latency budget
				if m.config.ARQDeadlineCheck && m.config.LatencyBudgetMs > 0 {
					paths := ps.pathTrack.GetAll()
					canArrive := false
					budget := time.Duration(m.config.LatencyBudgetMs) * time.Millisecond
					for _, p := range paths {
						if p.RTT > 0 && p.RTT < budget {
							canArrive = true
							break
						}
					}
					if !canArrive && len(paths) > 0 {
						continue // retransmit would exceed budget
					}
				}
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
		return ps.reorderBuf.InsertAt(packet, nonce, pathID, now)
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
				ps.sendFunc(data)
			}
		}
		if m.logger != nil && len(nonces) > 0 {
			m.logger.Printf("007 Bond: retransmitted %d packets on NACK", len(nonces))
		}

	case controlTypeProbe:
		// Receiver got a probe — echo it back
		echo := buildEchoPacket(packet)
		if echo != nil && ps.sendFunc != nil {
			ps.sendFunc(echo)
		}

	case controlTypeEcho:
		// Sender got an echo — compute RTT
		tsNano, pathID, ok := parseProbeEcho(packet)
		if ok {
			rtt := time.Duration(uint64(time.Now().UnixNano()) - tsNano)
			ps.pathTrack.UpdateRTT(pathID, rtt)
			// Feed RTT into reorder buffer for per-path timeout
			if ps.reorderBuf != nil {
				ps.reorderBuf.UpdatePathRTT(pathID, rtt)
			}
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
	flushTicker := time.NewTicker(time.Duration(m.config.ReorderFlushMs) * time.Millisecond)
	adaptTicker := time.NewTicker(time.Duration(m.config.ReorderAdaptSec) * time.Second)
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
			// Adapt window for all peers + update system state
			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.reorderBuf != nil {
					ps.reorderBuf.AdaptWindow()
				}
			}
			m.updateSystemState()
			m.peersMu.Unlock()
		}
	}
}

// updateSystemState computes overall system health from per-peer path states.
// Caller must hold m.peersMu.
func (m *Manager) updateSystemState() {
	var totalPaths, healthy, degraded, failed int
	for _, ps := range m.peers {
		if ps.pathTrack == nil {
			continue
		}
		for _, p := range ps.pathTrack.GetAll() {
			totalPaths++
			switch p.State {
			case PathHealthy, PathRecovering:
				healthy++
			case PathDegraded:
				degraded++
			case PathUnstable, PathFailed:
				failed++
			}
		}
	}

	var state SystemState
	switch {
	case totalPaths == 0 || healthy == totalPaths:
		state = SystemHealthy
	case healthy > 0:
		state = SystemDegraded
	case degraded > 0:
		state = SystemSeverelyDegraded
	default:
		state = SystemUnrecoverable
	}
	m.systemState.Store(int32(state))
}

// probeLoop periodically sends probe packets for RTT measurement.
func (m *Manager) probeLoop() {
	ticker := time.NewTicker(time.Duration(m.config.ProbeIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.sendFunc != nil {
					// Send probe on default path (pathID 0)
					probe := buildProbePacket(0)
					ps.sendFunc(probe)
				}
			}
			m.peersMu.Unlock()
		}
	}
}

// fecAdaptLoop periodically adjusts the FEC ratio based on measured loss.
func (m *Manager) fecAdaptLoop() {
	ticker := time.NewTicker(time.Duration(m.config.FECAdaptIntervalMs) * time.Millisecond)
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
	SystemState      string
	TxPackets        uint64
	RxPackets        uint64
	DropPackets      uint64
	FECRecovered     uint64
	FECFailed        uint64
	ReorderInOrder   uint64
	ReorderReordered uint64
	ReorderGaps      uint64
	ReorderWindowMs  int64
	Paths            []PathHealthSnapshot
}

// GetStats returns current statistics aggregated across all peers.
func (m *Manager) GetStats() Stats {
	s := Stats{
		SystemState: SystemState(m.systemState.Load()).String(),
		TxPackets:   m.txPackets.Load(),
		RxPackets:   m.rxPackets.Load(),
		DropPackets: m.dropPackets.Load(),
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
		if ps.pathTrack != nil {
			s.Paths = append(s.Paths, ps.pathTrack.GetAll()...)
		}
	}
	m.peersMu.Unlock()

	return s
}

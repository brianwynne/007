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
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Logger provides structured logging for the bond system.
// All log calls include key-value context fields for diagnostics.
type Logger interface {
	Info(msg string, keysAndValues ...interface{})
	Warn(msg string, keysAndValues ...interface{})
	Error(msg string, keysAndValues ...interface{})
}

// stdLogger wraps a standard log.Logger to implement Logger.
type stdLogger struct {
	l *log.Logger
}

func (s *stdLogger) Info(msg string, kv ...interface{})  { s.log("INFO", msg, kv) }
func (s *stdLogger) Warn(msg string, kv ...interface{})  { s.log("WARN", msg, kv) }
func (s *stdLogger) Error(msg string, kv ...interface{}) { s.log("ERROR", msg, kv) }

func (s *stdLogger) log(level, msg string, kv []interface{}) {
	if s.l == nil {
		return
	}
	if len(kv) == 0 {
		s.l.Printf("[%s] 007 Bond: %s", level, msg)
		return
	}
	fields := ""
	for i := 0; i+1 < len(kv); i += 2 {
		fields += fmt.Sprintf(" %v=%v", kv[i], kv[i+1])
	}
	s.l.Printf("[%s] 007 Bond: %s%s", level, msg, fields)
}

// NewStdLogger wraps a standard log.Logger for use with the bond Manager.
func NewStdLogger(l *log.Logger) Logger {
	return &stdLogger{l: l}
}

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
	FECMode         string       // "block" (default) or "sliding"
	FECAdaptive     bool         // dynamically adjust FEC ratio (block mode only)
	SlidingWindowSize int        // sliding mode: window size W (default 5)
	FECBlockTimeoutMs int        // max ms to wait for complete FEC block
	FECLossWindow   int          // packets in loss measurement sliding window
	FECAdaptIntervalMs int       // ms between FEC ratio adaptation
	FECLowK, FECLowM   int      // clean network FEC ratio
	FECMedK, FECMedM   int      // moderate loss FEC ratio
	FECHighK, FECHighM int      // high loss FEC ratio
	FECLowThreshold    float64  // loss rate threshold for low→med
	FECHighThreshold   float64  // loss rate threshold for med→high

	// Jitter buffer (replaces reorder buffer)
	JitterEnabled     bool       // enable jitter buffer
	JitterDepthMs     int        // 0 = auto-derive from budget - FEC fill time

	// Reorder (deprecated — use JitterEnabled instead)
	ReorderEnabled    bool       // enable reorder buffer (legacy)
	ReorderBufSize    int
	ReorderWindowMs   int
	ReorderMinMs      int
	ReorderMaxMs      int
	ReorderFlushMs    int
	ReorderAdaptSec   int

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

// DefaultConfig returns the field preset.
func DefaultConfig() Config {
	return FieldPreset()
}

// BroadcastPreset returns configuration for live broadcast contribution.
// 40ms total latency. K=2, M=2. Jitter buffer = 20ms (1 slot).
// Use when latency is critical and links are reasonable.
func BroadcastPreset() Config {
	return Config{
		Enabled:         true,
		LatencyBudgetMs: 40,
		PacketIntervalMs: 20,

		FECEnabled:  true,
		FECMode:     "sliding", // "sliding" or "block" — sliding is default
		FECAdaptive: false,
		SlidingWindowSize: DefaultSlidingWindow,
		FECLowK: 2, FECLowM: 2,
		FECMedK: 2, FECMedM: 2,
		FECHighK: 2, FECHighM: 2,
		FECBlockTimeoutMs: 0,
		FECLossWindow:     100,
		FECAdaptIntervalMs: 500,
		FECLowThreshold:  0.01,
		FECHighThreshold: 0.05,

		JitterEnabled: true,
		// JitterDepthMs: 0 = auto: 40 - (2-1)*20 = 20ms

		ARQEnabled:       true,
		ARQBufSize:       512,
		ARQMaxNonces:     32,
		ARQRateLimitMs:   10,
		ARQDeadlineCheck: true,

		ProbeIntervalMs:  1000,
		PathStatsWindow:  100,
	}
}

// StudioPreset returns configuration for studio-quality links.
// 80ms total latency. K=2, M=2. Jitter buffer = 60ms (3 slots).
// Use for studio-to-studio or managed network links.
func StudioPreset() Config {
	cfg := BroadcastPreset()
	cfg.LatencyBudgetMs = 80
	// JitterDepthMs: auto: 80 - (2-1)*20 = 60ms
	return cfg
}

// FieldPreset returns configuration for field contribution.
// 200ms total latency. K=2, M=4. Jitter buffer = 180ms (9 slots).
// Use for outdoor broadcast over WiFi + cellular.
func FieldPreset() Config {
	cfg := BroadcastPreset()
	cfg.LatencyBudgetMs = 200
	cfg.FECLowM = 4
	cfg.FECMedM = 4
	cfg.FECHighM = 4
	// JitterDepthMs: auto: 200 - (2-1)*20 = 180ms
	return cfg
}

// peerState holds per-peer FEC, jitter buffer, ARQ, and path health state.
type peerState struct {
	peerID     uint32              // peer identifier (for SetPeerPreset)
	encoder    *FECEncoder         // block FEC encoder
	decoder    *FECDecoder         // block FEC decoder
	slidingEnc *SlidingFECEncoder  // sliding-window FEC encoder
	slidingDec *SlidingFECDecoder  // sliding-window FEC decoder
	reorderBuf *ReorderBuffer      // legacy — used when JitterEnabled=false
	jitterBuf  *JitterBuffer       // playout-deadline-aware buffer
	retransmit        *retransmitBuffer
	nackTrack         *nackTracker
	pathTrack         *pathTracker
	sendFunc          func(data []byte)
	lastNACKProcessed time.Time
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
	txPackets        atomic.Uint64
	rxPackets        atomic.Uint64
	dropPackets      atomic.Uint64
	duplicatePackets atomic.Uint64
	nacksSent        atomic.Uint64
	nacksReceived    atomic.Uint64
	arqRetransmitOK  atomic.Uint64
	arqRetransmitMiss atomic.Uint64
	arqReceived      atomic.Uint64 // retransmissions received from peer
	arqDeadlineSkip  atomic.Uint64
	probeCount       int

	// Lifecycle
	running atomic.Bool
	cancel  context.CancelFunc
	ctx     context.Context

	// Logger — structured with key-value context
	logger Logger
}

// NewManager creates a new bond manager with the given configuration.
// If logger is nil, a no-op logger is used.
func NewManager(cfg Config, logger Logger) (*Manager, error) {
	if logger == nil {
		logger = &stdLogger{}
	}
	m := &Manager{
		config: cfg,
		peers:  make(map[uint32]*peerState),
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
		ps = &peerState{peerID: peerID}
		if m.config.FECEnabled {
			switch m.config.FECMode {
			case "sliding":
				ps.slidingEnc = NewSlidingFECEncoder(m.config.SlidingWindowSize)
				ps.slidingDec = NewSlidingFECDecoder(DefaultMaxRepairs, DefaultRepairMaxAge)
				m.logger.Info("sliding FEC created", "peer", peerID, "window", m.config.SlidingWindowSize)
			default: // "block" or empty
				enc, err := NewFECEncoder(m.config)
				if err != nil {
					m.logger.Error("failed to create FEC encoder", "peer", peerID, "err", err)
				} else {
					ps.encoder = enc
					blockTimeout := m.config.FECBlockTimeoutMs
					if blockTimeout <= 0 {
						blockTimeout = m.config.FECLowK*m.config.PacketIntervalMs + 50
					}
					if m.config.LatencyBudgetMs > 0 && blockTimeout > m.config.LatencyBudgetMs {
						blockTimeout = m.config.LatencyBudgetMs
					}
					ps.decoder = NewFECDecoder(blockTimeout, 256)
				}
			}
		}
		if m.config.JitterEnabled {
			// Derive jitter buffer depth: budget - FEC worst-case recovery
			depthMs := m.config.JitterDepthMs
			if depthMs <= 0 && m.config.LatencyBudgetMs > 0 {
				fecFill := (m.config.FECLowK - 1) * m.config.PacketIntervalMs
				depthMs = m.config.LatencyBudgetMs - fecFill
				if depthMs < m.config.PacketIntervalMs {
					depthMs = m.config.PacketIntervalMs
				}
			}
			if depthMs <= 0 {
				depthMs = 60 // fallback
			}
			ps.jitterBuf = NewJitterBuffer(JitterConfig{
				BufferDepth:    time.Duration(depthMs) * time.Millisecond,
				PacketInterval: time.Duration(m.config.PacketIntervalMs) * time.Millisecond,
				DeliverFunc:    nil, // set later via SetTUNWriter
			})
			m.logger.Info("jitter buffer created",
				"depth_ms", depthMs,
				"slots", depthMs/m.config.PacketIntervalMs,
				"budget_ms", m.config.LatencyBudgetMs)
		} else if m.config.ReorderEnabled {
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

// SetTUNWriter provides the callback for delivering packets to the TUN device.
// Called from the jitter buffer's playout goroutine at fixed intervals.
// Must be set before traffic flows if JitterEnabled is true.
func (m *Manager) SetTUNWriter(peerID uint32, fn func([]byte)) {
	ps := m.getPeerState(peerID)
	m.peersMu.Lock()
	if ps.jitterBuf != nil {
		ps.jitterBuf.deliverFunc = fn
	}
	m.peersMu.Unlock()
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

	m.ctx, m.cancel = context.WithCancel(context.Background())

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

	m.logger.Info("manager started",
		"fec", m.config.FECEnabled,
		"jitter", m.config.JitterEnabled,
		"reorder", m.config.ReorderEnabled,
		"arq", m.config.ARQEnabled,
		"latency_budget_ms", m.config.LatencyBudgetMs)
}

// SetPreset changes the latency preset at runtime without restart.
// Updates config and adjusts jitter buffer depth on all peer states.
func (m *Manager) SetPreset(name string) error {
	var cfg Config
	switch name {
	case "broadcast":
		cfg = BroadcastPreset()
	case "studio":
		cfg = StudioPreset()
	case "field":
		cfg = FieldPreset()
	default:
		return fmt.Errorf("unknown preset: %s (use broadcast, studio, or field)", name)
	}

	// Preserve runtime overrides
	cfg.FECMode = m.config.FECMode
	cfg.FECEnabled = m.config.FECEnabled
	cfg.ARQEnabled = m.config.ARQEnabled
	cfg.JitterEnabled = m.config.JitterEnabled

	// Update config
	m.config = cfg

	// Calculate new jitter buffer depth
	depthMs := cfg.LatencyBudgetMs
	if cfg.FECEnabled {
		fecFill := (cfg.FECLowK - 1) * cfg.PacketIntervalMs
		depthMs -= fecFill
	}
	if depthMs < cfg.PacketIntervalMs {
		depthMs = cfg.PacketIntervalMs
	}
	depth := time.Duration(depthMs) * time.Millisecond

	// Update all active jitter buffers
	m.peersMu.Lock()
	for _, ps := range m.peers {
		if ps.jitterBuf != nil {
			ps.jitterBuf.SetDepth(depth)
		}
	}
	m.peersMu.Unlock()

	m.logger.Info("preset changed",
		"preset", name,
		"latency_budget_ms", cfg.LatencyBudgetMs,
		"jitter_depth_ms", depthMs)

	// Signal peers so they change their jitter buffer for us
	m.peersMu.Lock()
	peerCount := len(m.peers)
	sentCount := 0
	for id, ps := range m.peers {
		if ps.sendFunc != nil {
			ps.sendFunc(buildPresetPacket(name))
			sentCount++
			m.logger.Info("preset control packet sent", "peer", id)
		} else {
			m.logger.Warn("peer has no sendFunc", "peer", id)
		}
	}
	m.peersMu.Unlock()
	m.logger.Info("preset signalled", "peers", peerCount, "sent", sentCount)

	return nil
}

// SetPeerPreset changes the jitter buffer depth for a single peer.
// Called when a peer signals its preset via a control packet.
func (m *Manager) SetPeerPreset(peerID uint32, name string) error {
	var budget int
	switch name {
	case "broadcast":
		budget = 40
	case "studio":
		budget = 80
	case "field":
		budget = 200
	default:
		return fmt.Errorf("unknown preset: %s", name)
	}

	fecFill := (m.config.FECLowK - 1) * m.config.PacketIntervalMs
	depthMs := budget - fecFill
	if depthMs < m.config.PacketIntervalMs {
		depthMs = m.config.PacketIntervalMs
	}
	depth := time.Duration(depthMs) * time.Millisecond

	m.peersMu.Lock()
	ps, ok := m.peers[peerID]
	m.peersMu.Unlock()

	if !ok {
		return fmt.Errorf("peer %d not found", peerID)
	}

	if ps.jitterBuf != nil {
		ps.jitterBuf.SetDepth(depth)
		m.logger.Info("peer preset changed",
			"peer", peerID,
			"preset", name,
			"jitter_depth_ms", depthMs)
	}

	return nil
}

// Stop shuts down background goroutines.
func (m *Manager) Stop() {
	if !m.running.Swap(false) {
		return
	}
	m.cancel()
	m.logger.Info("manager stopped")
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

	// Non-UDP traffic (TCP, ICMP) bypasses the entire bond pipeline.
	// No FEC encoding, no dataSeq, no repair packets. Just send raw.
	// Multi-path still works (SendBuffers sends on all paths).
	// TCP has its own retransmission and ordering.
	if isNonUDP(packet) {
		return [][]byte{packet}
	}

	// Sliding-window FEC path (UDP/RTP only)
	if m.config.FECEnabled && ps.slidingEnc != nil {
		encodedData, repairPkt, dataSeq := ps.slidingEnc.Encode(packet, nonce)
		ps.retransmit.Store(packet, dataSeq)
		return [][]byte{encodedData, repairPkt}
	}

	// Block FEC path (UDP/RTP only)
	if !m.config.FECEnabled || ps.encoder == nil {
		ps.retransmit.Store(packet, nonce)
		return [][]byte{packet}
	}

	encodedData, parityPackets, dataSeq := ps.encoder.Encode(packet, nonce)
	ps.retransmit.Store(packet, dataSeq)

	result := make([][]byte, 0, 1+len(parityPackets))
	result = append(result, encodedData)
	result = append(result, parityPackets...)
	return result
}

// ProcessInbound handles a packet on the receive path.
// Called after decryption and replay filter validation, before TUN write.
//
// isNonUDP returns true for valid IP packets that are NOT UDP.
// These bypass the entire bond recovery pipeline (FEC, ARQ, jitter buffer).
// Only UDP (RTP audio) needs the full recovery chain.
// isRawIPPacket validates that a packet is a genuine IP packet by checking
// both the version nibble AND the total length field. This prevents FEC
// packets from being misclassified when their blockID high byte collides
// with IPv4/IPv6 version nibbles (e.g. blockID 0x4500 looks like IPv4).
func isRawIPPacket(pkt []byte) bool {
	if len(pkt) < 20 {
		return false
	}
	switch pkt[0] >> 4 {
	case 4:
		ihl := int(pkt[0] & 0x0F)
		if ihl < 5 {
			return false
		}
		totalLen := int(pkt[2])<<8 | int(pkt[3])
		// totalLen must be valid (>= 20 byte header) and not exceed packet.
		// Use <= because WireGuard may pad packets for privacy.
		return totalLen >= 20 && totalLen <= len(pkt)
	case 6:
		if len(pkt) < 40 {
			return false
		}
		payloadLen := int(pkt[4])<<8 | int(pkt[5])
		return payloadLen+40 >= 40 && payloadLen+40 <= len(pkt)
	default:
		return false
	}
}

// Returns false for non-IP packets, short packets, and UDP — those go
// through the normal pipeline.
func isNonUDP(pkt []byte) bool {
	switch pkt[0] >> 4 {
	case 4: // IPv4: protocol at offset 9
		if len(pkt) < 20 {
			return false // too short for valid IPv4
		}
		return pkt[9] != 17 // true for TCP(6), ICMP(1), etc.
	case 6: // IPv6: next header at offset 6
		if len(pkt) < 40 {
			return false
		}
		return pkt[6] != 17
	default:
		return false // not IP — don't bypass
	}
}

// Pipeline: FEC decode → reorder data packets → deliver recovered immediately.
// Returns IP packets ready for TUN delivery, or nil if buffered.
func (m *Manager) ProcessInbound(peerID uint32, packet []byte, nonce uint64, pathID int) [][]byte {
	m.rxPackets.Add(1)
	if m.rxPackets.Load()%500 == 1 {
		hdr := byte(0)
		if len(packet) > 0 {
			hdr = packet[0]
		}
		m.logger.Info("ProcessInbound sample", "peerID", peerID, "len", len(packet), "hdr", hdr, "nonce", nonce, "pathID", pathID)
	}

	ps := m.getPeerState(peerID)

	// Single timestamp for all operations on this packet
	now := time.Now()

	// Track per-path receive stats
	ps.pathTrack.RecordReceive(pathID)

	// Check for control packets (NACK, retransmit, etc.)
	if isControlPacket(packet) {
		return m.handleControl(ps, packet, pathID)
	}

	// Raw IP packets (TCP, ICMP) bypass the entire recovery pipeline.
	// Uses IP total length validation to distinguish raw IP from FEC packets
	// whose blockID can collide with IPv4 version nibble (block FEC).
	if isRawIPPacket(packet) && isNonUDP(packet) {
		return [][]byte{packet}
	}

	// Sliding-window FEC decode
	if m.config.FECEnabled && ps.slidingDec != nil && IsSlidingFECPacket(packet) {
		data, recovered, missing := ps.slidingDec.Decode(packet)

		var result [][]byte

		if ps.jitterBuf != nil {
			// Step 1: Fire ARQ for gaps detected by FEC sequence tracking.
			// The decoder reports missing sequences when a data packet
			// arrives with a gap — NACKed immediately to race FEC.
			if m.config.ARQEnabled && len(missing) > 0 {
				for _, seq := range missing {
					ps.nackTrack.AddMissing(seq)
				}
				m.triggerNACK(ps)
			}

			// Step 2: Insert data packet
			if data != nil {
				if isControlPacket(data.Data) {
					m.handleControl(ps, data.Data, pathID)
				} else if isNonUDP(data.Data) {
					// TCP/ICMP — deliver immediately, skip jitter buffer
					result = append(result, data.Data)
					ps.jitterBuf.Skip(data.DataSeq)
				} else {
					ps.jitterBuf.Insert(data.Data, data.DataSeq, sourceData)
				}
			}

			// Step 3: Drain additional jitter buffer gaps
			if m.config.ARQEnabled {
				earlyNonces := ps.jitterBuf.DrainEarlyNACK()
				for _, n := range earlyNonces {
					if m.config.ARQDeadlineCheck {
						deadline := ps.jitterBuf.PlayoutDeadline(n)
						if !deadline.IsZero() {
							remaining := time.Until(deadline)
							paths := ps.pathTrack.GetAll()
							canArrive := false
							for _, p := range paths {
								if p.RTT > 0 && p.RTT < remaining {
									canArrive = true
									break
								}
							}
							if !canArrive && len(paths) > 0 {
								m.arqDeadlineSkip.Add(1)
								continue
							}
						}
					}
					ps.nackTrack.AddMissing(n)
				}
				if len(earlyNonces) > 0 {
					m.triggerNACK(ps)
				}
			}

			// Insert FEC-recovered packets (fills gaps that ARQ is already racing)
			for _, rec := range recovered {
				if isControlPacket(rec.Data) {
					m.handleControl(ps, rec.Data, pathID)
				} else if isNonUDP(rec.Data) {
					result = append(result, rec.Data)
					ps.jitterBuf.Skip(rec.DataSeq)
				} else {
					ps.jitterBuf.Insert(rec.Data, rec.DataSeq, sourceFEC)
				}
			}
			return result
		}

		// No jitter buffer — return directly
		if data != nil {
			result = append(result, data.Data)
		}
		for _, rec := range recovered {
			result = append(result, rec.Data)
		}
		return result
	}

	// Block FEC decode
	if m.config.FECEnabled && ps.decoder != nil && len(packet) > FECHeaderSize {
		data, recovered := ps.decoder.Decode(packet)

		var result [][]byte

		// --- JITTER BUFFER PATH ---
		if ps.jitterBuf != nil {
			// Insert data packet — control and non-UDP bypass the jitter buffer.
			if data != nil {
				if isControlPacket(data.Data) {
					m.handleControl(ps, data.Data, pathID)
				} else if isNonUDP(data.Data) {
					result = append(result, data.Data)
					ps.jitterBuf.Skip(data.DataSeq)
				} else {
					ps.jitterBuf.Insert(data.Data, data.DataSeq, sourceData)
				}
			}
			// Step 2: Fire ARQ BEFORE inserting FEC-recovered packets.
			// ARQ races FEC — NACK every gap immediately. If FEC recovers
			// the packet, the retransmit arrives as a harmless duplicate.
			// If FEC fails, ARQ is already in flight.
			if m.config.ARQEnabled {
				earlyNonces := ps.jitterBuf.DrainEarlyNACK()
				for _, n := range earlyNonces {
					if m.config.ARQDeadlineCheck {
						deadline := ps.jitterBuf.PlayoutDeadline(n)
						if !deadline.IsZero() {
							remaining := time.Until(deadline)
							paths := ps.pathTrack.GetAll()
							canArrive := false
							for _, p := range paths {
								if p.RTT > 0 && p.RTT < remaining {
									canArrive = true
									break
								}
							}
							if !canArrive && len(paths) > 0 {
								m.arqDeadlineSkip.Add(1)
								continue
							}
						}
					}
					ps.nackTrack.AddMissing(n)
				}
				if len(earlyNonces) > 0 {
					m.triggerNACK(ps)
				}
			}
			// Insert FEC-recovered packets (fills gaps that ARQ is already racing)
			for _, rec := range recovered {
				if isControlPacket(rec.Data) {
					m.handleControl(ps, rec.Data, pathID)
				} else if isNonUDP(rec.Data) {
					result = append(result, rec.Data)
					ps.jitterBuf.Skip(rec.DataSeq)
				} else {
					ps.jitterBuf.Insert(rec.Data, rec.DataSeq, sourceFEC)
				}
			}
			// Jitter buffer delivers via callback — return nil
			return result
		}

		// --- LEGACY REORDER BUFFER PATH ---
		// Data packet → reorder using its dataSeq
		if data != nil {
			if m.config.ReorderEnabled && ps.reorderBuf != nil {
				result = append(result, ps.reorderBuf.InsertAt(data.Data, data.DataSeq, pathID, now)...)
			} else {
				result = append(result, data.Data)
			}
		}
		// Recovered packets
		for _, rec := range recovered {
			if m.config.ReorderEnabled && ps.reorderBuf != nil {
				result = append(result, ps.reorderBuf.InsertAt(rec.Data, rec.DataSeq, pathID, now)...)
			} else {
				result = append(result, rec.Data)
			}
		}
		// Legacy reorder buffer NACK logic
		if ps.reorderBuf != nil {
			if m.config.ARQEnabled {
				earlyNonces := ps.reorderBuf.DrainEarlyNACK()
				for _, n := range earlyNonces {
					ps.nackTrack.AddMissing(n)
				}
				if len(earlyNonces) > 0 {
					m.triggerNACK(ps)
				}
			}
			skipped := ps.reorderBuf.DrainSkippedNonces()
			for range skipped {
				ps.pathTrack.RecordLoss(pathID)
			}
			for _, n := range skipped {
				if !m.config.ARQEnabled {
					continue
				}
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
						m.arqDeadlineSkip.Add(1)
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
func (m *Manager) handleControl(ps *peerState, packet []byte, pathID int) [][]byte {
	if len(packet) < FECHeaderSize {
		return nil
	}
	controlType := packet[2]
	m.logger.Info("handleControl", "type", controlType, "len", len(packet))

	switch controlType {
	case controlTypeNACK:
		// Sender-side rate limit on NACK processing (prevents amplification)
		now := time.Now()
		if now.Sub(ps.lastNACKProcessed) < time.Duration(m.config.ARQRateLimitMs)*time.Millisecond {
			return nil
		}
		ps.lastNACKProcessed = now

		m.nacksReceived.Add(1)
		seqs := parseNACKPacket(packet)
		if ps.sendFunc == nil {
			return nil
		}
		sent := 0
		for _, seq := range seqs {
			data := ps.retransmit.Lookup(seq)
			if data != nil {
				ps.sendFunc(buildRetransmitPacket(seq, data))
				m.arqRetransmitOK.Add(1)
				sent++
			} else {
				m.arqRetransmitMiss.Add(1)
			}
		}
		if sent > 0 {
			m.logger.Info("retransmitted on NACK", "count", sent, "requested", len(seqs), "missed", len(seqs)-sent)
		}

	case controlTypeRetransmit:
		seq, payload := parseRetransmitPacket(packet)
		if payload != nil {
			m.arqReceived.Add(1)
			if ps.jitterBuf != nil {
				if isNonUDP(payload) {
					ps.jitterBuf.Skip(seq)
					return [][]byte{payload}
				}
				ps.jitterBuf.Insert(payload, seq, sourceARQ)
				return nil
			} else if ps.reorderBuf != nil {
				return ps.reorderBuf.InsertAt(payload, seq, pathID, time.Now())
			}
			return [][]byte{payload}
		}

	case controlTypeProbe:
		echo := buildEchoPacket(packet)
		if echo != nil && ps.sendFunc != nil {
			ps.sendFunc(echo)
		}

	case controlTypeEcho:
		tsNano, pID, ok := parseProbeEcho(packet)
		if ok {
			rtt := time.Duration(uint64(time.Now().UnixNano()) - tsNano)
			ps.pathTrack.UpdateRTT(pID, rtt)
			if ps.reorderBuf != nil {
				ps.reorderBuf.UpdatePathRTT(pID, rtt)
			}
		}

	case controlTypePreset:
		preset := parsePresetPacket(packet)
		m.logger.Info("received preset control packet", "preset", preset, "preset_len", len(preset), "peer", ps.peerID, "pkt_len", len(packet))
		if preset != "" {
			if err := m.SetPeerPreset(ps.peerID, preset); err != nil {
				m.logger.Error("SetPeerPreset failed", "error", err)
			}
		}
	}
	return nil
}

// triggerNACK generates and sends a NACK for unrecoverable gaps.
func (m *Manager) triggerNACK(ps *peerState) {
	nack := ps.nackTrack.GenerateNACK()
	if nack == nil || ps.sendFunc == nil {
		return
	}
	ps.sendFunc(nack)
	m.nacksSent.Add(1)
}

// reorderLoop runs periodic flush and window adaptation for all peers.
func (m *Manager) reorderLoop() {
	flushTicker := time.NewTicker(time.Duration(m.config.ReorderFlushMs) * time.Millisecond)
	adaptTicker := time.NewTicker(time.Duration(m.config.ReorderAdaptSec) * time.Second)
	defer flushTicker.Stop()
	defer adaptTicker.Stop()

	for {
		select {
		case <-m.ctx.Done():
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
			// Adapt window, log state changes, update system state
			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.reorderBuf != nil {
					ps.reorderBuf.AdaptWindow()
				}
				// Log path state transitions
				if ps.pathTrack != nil {
					for _, sc := range ps.pathTrack.DrainStateChanges() {
						m.logger.Warn("path state changed",
							"path_id", sc.PathID,
							"from", sc.From.String(),
							"to", sc.To.String(),
							"loss", sc.Loss,
							"rtt_ms", sc.RTT.Milliseconds())
					}
				}
			}
			prev := SystemState(m.systemState.Load())
			m.updateSystemState()
			curr := SystemState(m.systemState.Load())
			if curr != prev {
				m.logger.Warn("system state changed",
					"from", prev.String(),
					"to", curr.String())
			}
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
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// Collect send functions and paths under lock, send outside lock
			// to prevent deadlock if sendFunc blocks on a full queue
			type probeWork struct {
				fn    func([]byte)
				paths []PathHealthSnapshot
			}
			var work []probeWork
			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.sendFunc == nil {
					continue
				}
				work = append(work, probeWork{
					fn:    ps.sendFunc,
					paths: ps.pathTrack.GetAll(),
				})
			}
			m.peersMu.Unlock()

			for _, w := range work {
				if len(w.paths) == 0 {
					w.fn(buildProbePacket(0))
				} else {
					for _, p := range w.paths {
						w.fn(buildProbePacket(p.PathID))
					}
				}
			}

			// Log jitter buffer stats every 5 probe cycles
			m.probeCount++
			if m.probeCount%5 == 0 {
				m.peersMu.Lock()
				for _, ps := range m.peers {
					if ps.jitterBuf != nil {
						s := ps.jitterBuf.Stats()
						if s.Delivered > 0 || s.Late > 0 || s.Jumps > 0 {
							m.logger.Info("jitter stats",
								"delivered", s.Delivered,
								"late", s.Late,
								"jumps", s.Jumps,
								"misses", s.Misses,
								"dupes", s.Duplicates,
								"fec_fills", s.FECFills,
								"arq_fills", s.ARQFills,
								"depth_ms", s.DepthMs,
								"buf_size", s.BufferSize)
						}
					}
				}
				m.peersMu.Unlock()
			}
		}
	}
}

// fecAdaptLoop periodically adjusts the FEC ratio based on measured loss.
func (m *Manager) fecAdaptLoop() {
	ticker := time.NewTicker(time.Duration(m.config.FECAdaptIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// Per-peer FEC adaptation using per-path loss data
			m.peersMu.Lock()
			for _, ps := range m.peers {
				if ps.encoder == nil || ps.pathTrack == nil {
					continue
				}
				// Use max loss across all paths for this peer
				var maxLoss float64
				for _, p := range ps.pathTrack.GetAll() {
					if p.Loss > maxLoss {
						maxLoss = p.Loss
					}
				}
				prevK, prevM := ps.encoder.k, ps.encoder.m
				if err := ps.encoder.AdaptRate(maxLoss); err != nil {
					m.logger.Error("FEC adapt error", "err", err, "loss_rate", maxLoss)
				}
				if ps.encoder.k != prevK || ps.encoder.m != prevM {
					m.logger.Info("FEC ratio changed",
						"prev_k", prevK, "prev_m", prevM,
						"new_k", ps.encoder.k, "new_m", ps.encoder.m,
						"loss_rate", maxLoss)
				}
			}
			m.peersMu.Unlock()
		}
	}
}

// Stats returns current bond statistics.
type Stats struct {
	SystemState       string
	Preset            string
	LatencyBudgetMs   int
	TxPackets         uint64
	RxPackets         uint64
	DropPackets       uint64
	DuplicatePackets  uint64
	FECRecovered      uint64
	FECFailed         uint64
	ReorderInOrder    uint64
	ReorderReordered  uint64
	ReorderGaps       uint64
	ReorderDuplicates uint64
	ReorderLate       uint64
	ReorderWindowMs   int64
	NACKsSent         uint64
	NACKsReceived     uint64
	ARQRetransmitOK   uint64
	ARQRetransmitMiss uint64
	ARQReceived       uint64
	ARQDeadlineSkip   uint64
	JitterDelivered   uint64
	JitterLate        uint64
	JitterMisses      uint64
	JitterFECFills    uint64
	JitterARQFills    uint64
	JitterJumps       uint64
	Paths             []PathHealthSnapshot
}

// GetStats returns current statistics aggregated across all peers.
func (m *Manager) GetStats() Stats {
	// Derive preset name from latency budget
	preset := "field"
	switch m.config.LatencyBudgetMs {
	case 40:
		preset = "broadcast"
	case 80:
		preset = "studio"
	case 200:
		preset = "field"
	}

	s := Stats{
		SystemState:       SystemState(m.systemState.Load()).String(),
		Preset:            preset,
		LatencyBudgetMs:   m.config.LatencyBudgetMs,
		TxPackets:         m.txPackets.Load(),
		RxPackets:         m.rxPackets.Load(),
		DropPackets:       m.dropPackets.Load(),
		DuplicatePackets:  m.duplicatePackets.Load(),
		NACKsSent:         m.nacksSent.Load(),
		NACKsReceived:     m.nacksReceived.Load(),
		ARQRetransmitOK:   m.arqRetransmitOK.Load(),
		ARQRetransmitMiss: m.arqRetransmitMiss.Load(),
		ARQReceived:       m.arqReceived.Load(),
		ARQDeadlineSkip:   m.arqDeadlineSkip.Load(),
	}

	m.peersMu.Lock()
	for _, ps := range m.peers {
		if ps.decoder != nil {
			recovered, failed := ps.decoder.Stats()
			s.FECRecovered += recovered
			s.FECFailed += failed
		}
		if ps.slidingDec != nil {
			recovered, failed := ps.slidingDec.Stats()
			s.FECRecovered += recovered
			s.FECFailed += failed
		}
		if ps.reorderBuf != nil {
			inOrder, reordered, gaps, dups, late, windowMs := ps.reorderBuf.Stats()
			s.ReorderInOrder += inOrder
			s.ReorderReordered += reordered
			s.ReorderGaps += gaps
			s.ReorderDuplicates += dups
			s.ReorderLate += late
			s.DuplicatePackets += dups
			s.DropPackets += late // late packets are effectively dropped
			if windowMs > s.ReorderWindowMs {
				s.ReorderWindowMs = windowMs
			}
		}
		if ps.jitterBuf != nil {
			js := ps.jitterBuf.Stats()
			s.JitterDelivered += js.Delivered
			s.JitterLate += js.Late
			s.JitterMisses += js.Misses
			s.JitterFECFills += js.FECFills
			s.JitterARQFills += js.ARQFills
			s.JitterJumps += js.Jumps
		}
		if ps.pathTrack != nil {
			s.Paths = append(s.Paths, ps.pathTrack.GetAll()...)
		}
	}
	m.peersMu.Unlock()

	return s
}

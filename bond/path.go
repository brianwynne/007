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

// Path health tracking provides per-path RTT, loss, and jitter measurements
// using probe/echo control messages that travel through the WireGuard tunnel.
//
// Probe/echo flow:
//   1. Manager sends PROBE control packet (contains timestamp + pathID)
//   2. Receiver echoes it back as ECHO control packet
//   3. Sender receives ECHO, computes RTT = now - timestamp
//   4. RTT fed into reorder buffer for per-path timeout
//
// Per-path packet counting:
//   - Each received packet's pathID is tracked
//   - Loss estimated from gaps in nonce sequence per path
//   - Jitter computed from inter-arrival time variance
//
// Control packet types (via bond/arq.go infrastructure):
//   Type 2 = PROBE: [timestamp_ns (8)][pathID (4)]
//   Type 3 = ECHO:  [timestamp_ns (8)][pathID (4)]

const (
	controlTypeProbe = 2
	controlTypeEcho  = 3

	probeInterval    = 1 * time.Second // how often to send probes
	pathStatsWindow  = 100             // packets in loss measurement window
)

// PathState represents the explicit health state of a network path.
type PathState int

const (
	PathHealthy     PathState = iota // operating normally
	PathDegraded                      // elevated loss or jitter, still usable
	PathUnstable                      // flapping or very high loss
	PathFailed                        // no packets received recently
	PathRecovering                    // was failed, seeing packets again
)

func (s PathState) String() string {
	switch s {
	case PathHealthy:
		return "healthy"
	case PathDegraded:
		return "degraded"
	case PathUnstable:
		return "unstable"
	case PathFailed:
		return "failed"
	case PathRecovering:
		return "recovering"
	default:
		return "unknown"
	}
}

// PathHealth holds per-path health metrics.
type PathHealth struct {
	mu sync.Mutex

	PathID    int
	State     PathState     // explicit path state
	RTT       time.Duration // exponential moving average
	RTTVar    time.Duration // RTT variance (for jitter)
	Loss      float64       // estimated loss rate (0.0 - 1.0)
	BurstLoss int           // current consecutive loss streak
	MaxBurst  int           // max burst loss observed
	LastProbe time.Time     // when last probe was sent
	LastSeen  time.Time     // when last packet was received on this path
	RxCount   uint64        // total packets received on this path
	DropCount uint64        // packets dropped (late/duplicate/invalid)
	TxProbes  uint64        // probes sent on this path
	RxProbes  uint64        // probe echoes received

	// Inter-arrival jitter — computed from actual inter-arrival variance,
	// not a hardcoded expected interval
	lastArrival time.Time
	jitter      time.Duration
	avgInterval time.Duration // exponential moving average of actual inter-arrival

	// Loss tracking — sliding window of received/missed
	lossWindow []bool // true = received, false = gap
	lossIdx    int

	// State transition tracking
	stableCount  int       // consecutive stable checks for recovery
	failedSince  time.Time // when path entered failed state
	stateChanged bool      // set when state transitions (cleared by DrainStateChanges)
	prevState    PathState // previous state for logging
}

// pathTracker manages health metrics for all paths of a peer.
type pathTracker struct {
	mu    sync.Mutex
	paths map[int]*PathHealth
}

func newPathTracker() *pathTracker {
	return &pathTracker{
		paths: make(map[int]*PathHealth),
	}
}

// RecordReceive records a packet arrival on a path.
func (pt *pathTracker) RecordReceive(pathID int) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	ph := pt.getOrCreate(pathID)
	ph.mu.Lock()
	defer ph.mu.Unlock()

	now := time.Now()
	ph.RxCount++
	ph.LastSeen = now

	// Reset burst counter on receive
	ph.BurstLoss = 0

	// Inter-arrival jitter — computed from deviation against actual average
	// inter-arrival time, not a hardcoded constant. This avoids inflated
	// jitter from mixed traffic (keepalives every 25s + data at 50pps).
	if !ph.lastArrival.IsZero() {
		diff := now.Sub(ph.lastArrival)
		if ph.avgInterval == 0 {
			ph.avgInterval = diff
		} else {
			deviation := diff - ph.avgInterval
			if deviation < 0 {
				deviation = -deviation
			}
			ph.jitter = ph.jitter + (deviation-ph.jitter)/16
			ph.avgInterval = ph.avgInterval + (diff-ph.avgInterval)/16
		}
	}
	ph.lastArrival = now

	// Loss window
	if len(ph.lossWindow) < pathStatsWindow {
		ph.lossWindow = append(ph.lossWindow, true)
	} else {
		ph.lossWindow[ph.lossIdx] = true
		ph.lossIdx = (ph.lossIdx + 1) % pathStatsWindow
	}

	// Update path state
	ph.updateState()
}

// RecordLoss records a missed packet on a path.
func (pt *pathTracker) RecordLoss(pathID int) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	ph := pt.getOrCreate(pathID)
	ph.mu.Lock()
	defer ph.mu.Unlock()

	// Track burst loss
	ph.BurstLoss++
	if ph.BurstLoss > ph.MaxBurst {
		ph.MaxBurst = ph.BurstLoss
	}

	if len(ph.lossWindow) < pathStatsWindow {
		ph.lossWindow = append(ph.lossWindow, false)
	} else {
		ph.lossWindow[ph.lossIdx] = false
		ph.lossIdx = (ph.lossIdx + 1) % pathStatsWindow
	}

	// Compute loss rate
	lost := 0
	for _, received := range ph.lossWindow {
		if !received {
			lost++
		}
	}
	ph.Loss = float64(lost) / float64(len(ph.lossWindow))

	ph.updateState()
}

// UpdateRTT records a new RTT measurement for a path.
func (pt *pathTracker) UpdateRTT(pathID int, rtt time.Duration) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	ph := pt.getOrCreate(pathID)
	ph.mu.Lock()
	defer ph.mu.Unlock()

	ph.RxProbes++

	if ph.RTT == 0 {
		ph.RTT = rtt
		ph.RTTVar = rtt / 2
	} else {
		// TCP-style EWMA
		diff := ph.RTT - rtt
		if diff < 0 {
			diff = -diff
		}
		ph.RTTVar = time.Duration(0.75*float64(ph.RTTVar) + 0.25*float64(diff))
		ph.RTT = time.Duration(0.875*float64(ph.RTT) + 0.125*float64(rtt))
	}
}

// GetAll returns a snapshot of all path health metrics.
func (pt *pathTracker) GetAll() []PathHealthSnapshot {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	result := make([]PathHealthSnapshot, 0, len(pt.paths))
	for _, ph := range pt.paths {
		ph.mu.Lock()
		result = append(result, PathHealthSnapshot{
			PathID:    ph.PathID,
			State:     ph.State,
			RTT:       ph.RTT,
			RTTVar:    ph.RTTVar,
			Jitter:    ph.jitter,
			Loss:      ph.Loss,
			BurstLoss: ph.BurstLoss,
			MaxBurst:  ph.MaxBurst,
			RxCount:   ph.RxCount,
			DropCount: ph.DropCount,
		})
		ph.mu.Unlock()
	}
	return result
}

// updateState computes the path state from current metrics.
// Caller must hold ph.mu.
func (ph *PathHealth) updateState() {
	prev := ph.State
	defer func() {
		if ph.State != prev {
			ph.stateChanged = true
			ph.prevState = prev
		}
	}()
	switch {
	case ph.Loss > 0.50 || ph.BurstLoss > 20:
		if ph.State != PathFailed {
			ph.failedSince = time.Now()
		}
		ph.State = PathFailed
		ph.stableCount = 0
	case ph.Loss > 0.10 || ph.BurstLoss > 10:
		ph.State = PathUnstable
		ph.stableCount = 0
	case ph.Loss > 0.02:
		ph.State = PathDegraded
		ph.stableCount = 0
	default:
		if ph.State == PathFailed || ph.State == PathUnstable {
			ph.stableCount++
			if ph.stableCount < 10 {
				ph.State = PathRecovering
			} else {
				ph.State = PathHealthy
			}
		} else if ph.State == PathRecovering {
			ph.stableCount++
			if ph.stableCount >= 10 {
				ph.State = PathHealthy
			}
		} else {
			ph.State = PathHealthy
		}
	}
}

// RecordDrop increments the drop counter for a path.
func (pt *pathTracker) RecordDrop(pathID int) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	ph := pt.getOrCreate(pathID)
	ph.mu.Lock()
	ph.DropCount++
	ph.mu.Unlock()
}

// StateChange represents a path state transition for logging.
type StateChange struct {
	PathID int
	From   PathState
	To     PathState
	Loss   float64
	RTT    time.Duration
}

// DrainStateChanges returns and clears any pending state transitions.
func (pt *pathTracker) DrainStateChanges() []StateChange {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	var changes []StateChange
	for _, ph := range pt.paths {
		ph.mu.Lock()
		if ph.stateChanged {
			changes = append(changes, StateChange{
				PathID: ph.PathID,
				From:   ph.prevState,
				To:     ph.State,
				Loss:   ph.Loss,
				RTT:    ph.RTT,
			})
			ph.stateChanged = false
		}
		ph.mu.Unlock()
	}
	return changes
}

func (pt *pathTracker) getOrCreate(pathID int) *PathHealth {
	ph, ok := pt.paths[pathID]
	if !ok {
		ph = &PathHealth{
			PathID:     pathID,
			lossWindow: make([]bool, 0, pathStatsWindow),
		}
		pt.paths[pathID] = ph
	}
	return ph
}

// PathHealthSnapshot is a point-in-time view of path health.
type PathHealthSnapshot struct {
	PathID    int
	State     PathState
	RTT       time.Duration
	RTTVar    time.Duration
	Jitter    time.Duration
	Loss      float64
	BurstLoss int
	MaxBurst  int
	RxCount   uint64
	DropCount uint64
}

// buildProbePacket creates a probe control packet with a timestamp.
func buildProbePacket(pathID int) []byte {
	pkt := make([]byte, FECHeaderSize+12)
	binary.BigEndian.PutUint16(pkt[0:2], controlBlockID)
	pkt[2] = controlTypeProbe
	pkt[3] = 0 // K=0
	pkt[4] = 0 // M=0
	binary.BigEndian.PutUint64(pkt[FECHeaderSize:FECHeaderSize+8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(pkt[FECHeaderSize+8:FECHeaderSize+12], uint32(pathID))
	return pkt
}

// buildEchoPacket creates an echo response from a received probe.
func buildEchoPacket(probePayload []byte) []byte {
	if len(probePayload) < FECHeaderSize+12 {
		return nil
	}
	pkt := make([]byte, FECHeaderSize+12)
	binary.BigEndian.PutUint16(pkt[0:2], controlBlockID)
	pkt[2] = controlTypeEcho
	pkt[3] = 0
	pkt[4] = 0
	// Copy timestamp and pathID from probe
	copy(pkt[FECHeaderSize:], probePayload[FECHeaderSize:FECHeaderSize+12])
	return pkt
}

// parseProbeEcho extracts timestamp and pathID from a probe/echo packet.
func parseProbeEcho(pkt []byte) (timestampNs uint64, pathID int, ok bool) {
	if len(pkt) < FECHeaderSize+12 {
		return 0, 0, false
	}
	timestampNs = binary.BigEndian.Uint64(pkt[FECHeaderSize : FECHeaderSize+8])
	pathID = int(binary.BigEndian.Uint32(pkt[FECHeaderSize+8 : FECHeaderSize+12]))
	return timestampNs, pathID, true
}

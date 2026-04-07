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

// Sliding-window FEC using XOR-based repair packets.
//
// Each data packet generates one repair packet = XOR of the last W data packets.
// Windows overlap: packet N is protected by repairs from N, N+1, ..., N+W-1.
// Single loss recovery: immediate (20ms, one packet interval).
// Burst recovery: overlapping windows can recover bursts up to W-1 consecutive.
//
// Wire format:
//
//   Data:   [Type=0x01][Flags][DataSeq (8)][IP packet]
//   Repair: [Type=0x02][Flags][RepairSeq (8)][WindowStart (8)][WindowSize (1)][XOR data]

const (
	SlidingFECDataType   = 0x01
	SlidingFECRepairType = 0x02

	SlidingDataHeaderSize   = 10 // type(1) + flags(1) + dataSeq(8)
	SlidingRepairHeaderSize = 19 // type(1) + flags(1) + repairSeq(8) + windowStart(8) + windowSize(1)

	DefaultSlidingWindow  = 5   // 100ms at 50pps
	DefaultMaxRepairs     = 64
	DefaultRepairMaxAge   = 500 * time.Millisecond
)

// SlidingFECEncoder generates XOR repair packets over a sliding window.
type SlidingFECEncoder struct {
	mu         sync.Mutex
	windowSize int
	window     [][]byte  // circular buffer of last W padded data packets
	windowSeq  []uint64  // dataSeq for each window entry
	head       int
	count      int       // fill level (< windowSize during startup)
	dataSeq    uint64
	repairSeq  uint64
}

// NewSlidingFECEncoder creates a sliding-window FEC encoder.
func NewSlidingFECEncoder(windowSize int) *SlidingFECEncoder {
	if windowSize <= 0 {
		windowSize = DefaultSlidingWindow
	}
	return &SlidingFECEncoder{
		windowSize: windowSize,
		window:     make([][]byte, windowSize),
		windowSeq:  make([]uint64, windowSize),
	}
}

// Encode adds a data packet and returns the encoded data + a repair packet.
// Unlike block FEC, a repair is generated for EVERY data packet (1:1 ratio).
// Returns: encodedData, repairPacket, dataSeq.
func (e *SlidingFECEncoder) Encode(data []byte, nonce uint64) (encodedData []byte, repairPkt []byte, dataSeq uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	dataSeq = e.dataSeq
	e.dataSeq++

	// Build data packet: [type=0x01][flags=0][dataSeq(8)][IP data]
	encodedData = make([]byte, SlidingDataHeaderSize+len(data))
	encodedData[0] = SlidingFECDataType
	encodedData[1] = 0
	binary.BigEndian.PutUint64(encodedData[2:10], dataSeq)
	copy(encodedData[SlidingDataHeaderSize:], data)

	// Store padded copy in window
	stored := make([]byte, len(encodedData))
	copy(stored, encodedData)
	e.window[e.head] = stored
	e.windowSeq[e.head] = dataSeq
	e.head = (e.head + 1) % e.windowSize
	if e.count < e.windowSize {
		e.count++
	}

	// XOR all packets in window
	maxLen := 0
	for i := 0; i < e.count; i++ {
		if len(e.window[i]) > maxLen {
			maxLen = len(e.window[i])
		}
	}
	xorData := make([]byte, maxLen)
	for i := 0; i < e.count; i++ {
		for j := 0; j < len(e.window[i]); j++ {
			xorData[j] ^= e.window[i][j]
		}
	}

	// Window start = oldest dataSeq in current window
	windowStart := dataSeq - uint64(e.count) + 1

	// Build repair packet
	repairPkt = make([]byte, SlidingRepairHeaderSize+len(xorData))
	repairPkt[0] = SlidingFECRepairType
	repairPkt[1] = 0
	binary.BigEndian.PutUint64(repairPkt[2:10], e.repairSeq)
	binary.BigEndian.PutUint64(repairPkt[10:18], windowStart)
	repairPkt[18] = byte(e.count)
	copy(repairPkt[SlidingRepairHeaderSize:], xorData)
	e.repairSeq++

	return encodedData, repairPkt, dataSeq
}

// SlidingFECDecoder recovers lost packets using XOR repair packets.
type SlidingFECDecoder struct {
	mu         sync.Mutex
	received   map[uint64][]byte // dataSeq → full encoded data packet (for XOR)
	repairs    []*slidingRepair  // ring of recent repairs
	repairHead int
	maxRepairs int
	maxAge     time.Duration

	// Sequence tracking for gap detection — reports missing seqs to caller
	// so ARQ can fire immediately, racing FEC recovery.
	nextExpected   uint64
	seqInitialized bool

	recoveredCount uint64
	failedCount    uint64
}

type slidingRepair struct {
	xorData     []byte
	windowStart uint64
	windowSize  int
	repairSeq   uint64
	created     time.Time
}

// NewSlidingFECDecoder creates a sliding-window FEC decoder.
func NewSlidingFECDecoder(maxRepairs int, maxAge time.Duration) *SlidingFECDecoder {
	if maxRepairs <= 0 {
		maxRepairs = DefaultMaxRepairs
	}
	if maxAge <= 0 {
		maxAge = DefaultRepairMaxAge
	}
	return &SlidingFECDecoder{
		received:   make(map[uint64][]byte),
		repairs:    make([]*slidingRepair, maxRepairs),
		maxRepairs: maxRepairs,
		maxAge:     maxAge,
	}
}

// Decode processes an incoming sliding FEC packet (data or repair).
// Returns the data packet (for data type), any recovered packets, and
// sequences detected as missing (for immediate ARQ, racing FEC).
func (d *SlidingFECDecoder) Decode(packet []byte) (data *DecodedPacket, recovered []*DecodedPacket, missing []uint64) {
	if len(packet) < 2 {
		return nil, nil, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	switch packet[0] {
	case SlidingFECDataType:
		if len(packet) < SlidingDataHeaderSize {
			return nil, nil, nil
		}
		seq := binary.BigEndian.Uint64(packet[2:10])
		payload := make([]byte, len(packet)-SlidingDataHeaderSize)
		copy(payload, packet[SlidingDataHeaderSize:])
		data = &DecodedPacket{Data: payload, DataSeq: seq}

		// Detect gaps BEFORE storing — these are missing sequences that
		// ARQ should NACK immediately, racing FEC recovery.
		if !d.seqInitialized {
			d.nextExpected = seq + 1
			d.seqInitialized = true
		} else if seq > d.nextExpected {
			for s := d.nextExpected; s < seq; s++ {
				if _, ok := d.received[s]; !ok {
					missing = append(missing, s)
				}
			}
			d.nextExpected = seq + 1
		} else if seq >= d.nextExpected {
			d.nextExpected = seq + 1
		}

		// Store for future XOR recovery
		stored := make([]byte, len(packet))
		copy(stored, packet)
		d.received[seq] = stored

		// Check if any stored repair can now recover a missing packet
		recovered = d.tryRecoverAll()

	case SlidingFECRepairType:
		if len(packet) < SlidingRepairHeaderSize {
			return nil, nil, nil
		}
		repairSeq := binary.BigEndian.Uint64(packet[2:10])
		windowStart := binary.BigEndian.Uint64(packet[10:18])
		windowSize := int(packet[18])
		if windowSize <= 0 || windowSize > 32 {
			return nil, nil, nil // sanity check
		}
		xorData := make([]byte, len(packet)-SlidingRepairHeaderSize)
		copy(xorData, packet[SlidingRepairHeaderSize:])

		d.repairs[d.repairHead] = &slidingRepair{
			xorData:     xorData,
			windowStart: windowStart,
			windowSize:  windowSize,
			repairSeq:   repairSeq,
			created:     time.Now(),
		}
		d.repairHead = (d.repairHead + 1) % d.maxRepairs

		// Try immediate recovery
		recovered = d.tryRecoverAll()
	}

	d.evictOld()
	return data, recovered, missing
}

// tryRecoverAll checks all stored repairs for single-loss windows.
func (d *SlidingFECDecoder) tryRecoverAll() []*DecodedPacket {
	var recovered []*DecodedPacket
	for i, repair := range d.repairs {
		if repair == nil {
			continue
		}
		// Count missing in this window
		var missingSeq uint64
		missingCount := 0
		for j := 0; j < repair.windowSize; j++ {
			seq := repair.windowStart + uint64(j)
			if _, ok := d.received[seq]; !ok {
				missingSeq = seq
				missingCount++
				if missingCount > 1 {
					break // can't recover more than 1 per XOR
				}
			}
		}
		if missingCount != 1 {
			if missingCount == 0 {
				// All present — clean up this repair
				d.repairs[i] = nil
			}
			continue
		}

		// XOR repair with all present packets → recover missing
		result := make([]byte, len(repair.xorData))
		copy(result, repair.xorData)
		for j := 0; j < repair.windowSize; j++ {
			seq := repair.windowStart + uint64(j)
			if seq == missingSeq {
				continue
			}
			pkt := d.received[seq]
			for b := 0; b < len(pkt) && b < len(result); b++ {
				result[b] ^= pkt[b]
			}
		}

		// Extract dataSeq and payload from recovered packet
		if len(result) >= SlidingDataHeaderSize && result[0] == SlidingFECDataType {
			recSeq := binary.BigEndian.Uint64(result[2:10])
			payload := make([]byte, len(result)-SlidingDataHeaderSize)
			copy(payload, result[SlidingDataHeaderSize:])
			recovered = append(recovered, &DecodedPacket{
				Data:    payload,
				DataSeq: recSeq,
			})
			// Store recovered packet for future repairs
			d.received[recSeq] = result
			d.recoveredCount++
		}

		// This repair is consumed
		d.repairs[i] = nil
	}
	return recovered
}

// evictOld removes stale entries from received and repairs.
func (d *SlidingFECDecoder) evictOld() {
	now := time.Now()
	cutoff := now.Add(-d.maxAge)

	// Evict old repairs
	for i, r := range d.repairs {
		if r != nil && r.created.Before(cutoff) {
			d.repairs[i] = nil
		}
	}

	// Limit received map size (keep last 256 entries)
	if len(d.received) > 256 {
		// Find min seq to keep
		var minKeep uint64
		for seq := range d.received {
			if minKeep == 0 || seq < minKeep {
				minKeep = seq
			}
		}
		// Keep only the latest 128
		threshold := minKeep + uint64(len(d.received)) - 128
		for seq := range d.received {
			if seq < threshold {
				delete(d.received, seq)
			}
		}
	}
}

// Stats returns decoder statistics.
func (d *SlidingFECDecoder) Stats() (recovered, failed uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.recoveredCount, d.failedCount
}

// IsSlidingFECPacket checks if a packet uses the sliding FEC wire format.
func IsSlidingFECPacket(pkt []byte) bool {
	if len(pkt) < 2 {
		return false
	}
	return pkt[0] == SlidingFECDataType || pkt[0] == SlidingFECRepairType
}

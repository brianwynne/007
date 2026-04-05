/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

package bond

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
)

// FEC implements adaptive Forward Error Correction using Reed-Solomon codes.
//
// Design decisions:
//   - Uses klauspost/reedsolomon (production-ready, 1GB/s+, ARM optimised)
//   - Adaptive K,M: adjusts based on measured loss rate every 500ms
//   - FEC header (5 bytes) + nonce (8 bytes) prepended before encryption
//   - The nonce is part of the RS-encoded data, so it's recovered with the payload
//   - Encoder groups K packets, generates M parity packets
//   - Decoder recovers any M lost packets from remaining K
//
// Wire format per data packet:
//
//	[FEC header (5 bytes)][WireGuard nonce (8 bytes)][IP packet]
//
// FEC header:
//
//	[BlockID (16 bits)][Index (8 bits)][K (8 bits)][M (8 bits)]
//
// The nonce is embedded in the FEC-protected region so that recovered
// packets have their original nonce available for reorder buffer insertion.

const (
	FECHeaderSize    = 5 // block management header
	fecNonceSize     = 8 // WireGuard nonce (uint64)
	FECPayloadOffset = FECHeaderSize + fecNonceSize // 13 — where IP data starts
	FECOverhead      = FECPayloadOffset             // total bytes added to each data packet

	// Adaptive FEC presets
	fecLowLossK  = 16 // clean network: 12.5% overhead
	fecLowLossM  = 2
	fecMedLossK  = 12 // moderate loss: 25% overhead
	fecMedLossM  = 4
	fecHighLossK = 8 // high loss: 42% overhead
	fecHighLossM = 6

	// Loss thresholds for switching presets
	lowLossThreshold  = 0.01 // 1%
	highLossThreshold = 0.05 // 5%

	// Timing
	fecAdaptInterval  = 500 * time.Millisecond
	fecBlockTimeoutMs = 50  // max ms to wait for a complete FEC block
	fecLossWindowSize = 200 // packets in loss measurement window
)

// DecodedPacket holds a decoded packet with its data sequence number.
type DecodedPacket struct {
	Data    []byte // IP packet (FEC header + seq stripped)
	DataSeq uint64 // data-only sequence number for reorder buffer
}

// FECEncoder groups outgoing packets and generates parity packets.
type FECEncoder struct {
	mu sync.Mutex

	k, m    int // current data/parity ratio
	blockID uint16
	encoder reedsolomon.Encoder
	config  Config // configurable thresholds and ratios

	// Data sequence counter — increments only for data packets,
	// never for parity or control. Used for reorder buffer ordering
	// to avoid phantom gaps from parity nonces.
	dataSeq uint64

	// Current block being filled
	currentBlock [][]byte // K data packets
	blockIdx     int      // how many data packets collected

	// Loss measurement for adaptation
	txCount    uint64
	lossWindow []bool // sliding window of loss events
	lossIdx    int
}

// FECDecoder collects packets per block and recovers lost packets.
type FECDecoder struct {
	mu sync.Mutex

	// Active blocks waiting for completion
	blocks    map[uint16]*fecBlock
	maxBlocks int // cap on concurrent blocks (0 = 256 default)

	// Cached RS encoders — avoids expensive reedsolomon.New() on every recovery
	rsCache map[uint32]reedsolomon.Encoder // key: k<<16 | m

	// Block timeout
	blockTimeoutMs int // 0 = use fecBlockTimeoutMs constant

	// Stats
	recoveredCount uint64
	failedCount    uint64
}

type fecBlock struct {
	k, m     int
	shards   [][]byte  // K+M shards (nil = missing)
	present  []bool    // which shards have arrived
	received int       // count of received shards
	maxLen   int       // max shard length (for padding)
	created  time.Time // when first packet of this block arrived
	timer    *time.Timer
}

// NewFECEncoder creates a new adaptive FEC encoder with the given config.
func NewFECEncoder(cfg Config) (*FECEncoder, error) {
	k, m := cfg.FECLowK, cfg.FECLowM
	if k == 0 {
		k = fecLowLossK
	}
	if m == 0 {
		m = fecLowLossM
	}
	enc, err := reedsolomon.New(k, m)
	if err != nil {
		return nil, err
	}
	lossWindow := cfg.FECLossWindow
	if lossWindow == 0 {
		lossWindow = fecLossWindowSize
	}
	fe := &FECEncoder{
		k:            k,
		m:            m,
		encoder:      enc,
		currentBlock: make([][]byte, k),
		lossWindow:   make([]bool, lossWindow),
		config:       cfg,
	}
	return fe, nil
}

// Encode adds a data packet to the current FEC block.
// A data-only sequence number (dataSeq) is embedded in the FEC-protected
// payload. This counter only increments for data packets, never parity,
// so the reorder buffer sees a contiguous sequence with no phantom gaps.
// Returns encodedData, any parity packets, and the assigned dataSeq.
func (fe *FECEncoder) Encode(data []byte, nonce uint64) (encodedData []byte, parityPackets [][]byte, dataSeq uint64) {
	fe.mu.Lock()
	defer fe.mu.Unlock()

	dataSeq = fe.dataSeq
	fe.dataSeq++

	// Build: [FEC header (5)][dataSeq (8)][IP data]
	headerData := make([]byte, FECPayloadOffset+len(data))
	binary.BigEndian.PutUint16(headerData[0:2], fe.blockID)
	headerData[2] = byte(fe.blockIdx) // index within block
	headerData[3] = byte(fe.k)       // K value
	headerData[4] = byte(fe.m)       // M value
	binary.BigEndian.PutUint64(headerData[FECHeaderSize:FECPayloadOffset], dataSeq)
	copy(headerData[FECPayloadOffset:], data)

	fe.currentBlock[fe.blockIdx] = headerData
	fe.blockIdx++
	fe.txCount++

	encodedData = headerData

	// Block not full yet
	if fe.blockIdx < fe.k {
		return encodedData, nil, dataSeq
	}

	// Block full — generate parity
	parityPackets = fe.generateParity()

	// Advance to next block
	fe.blockID++
	fe.blockIdx = 0
	fe.currentBlock = make([][]byte, fe.k)

	return encodedData, parityPackets, dataSeq
}

// generateParity creates M parity packets from the current K data packets.
func (fe *FECEncoder) generateParity() [][]byte {
	// Pad all shards to same length
	maxLen := 0
	for _, shard := range fe.currentBlock {
		if len(shard) > maxLen {
			maxLen = len(shard)
		}
	}

	// Create full shard set: K data + M parity (all same length)
	shards := make([][]byte, fe.k+fe.m)
	for i := 0; i < fe.k; i++ {
		shards[i] = make([]byte, maxLen)
		copy(shards[i], fe.currentBlock[i])
	}
	for i := fe.k; i < fe.k+fe.m; i++ {
		shards[i] = make([]byte, maxLen)
	}

	// Generate parity
	if err := fe.encoder.Encode(shards); err != nil {
		return nil // encoding failed — send data without parity
	}

	// Build parity packets with FEC headers
	result := make([][]byte, fe.m)
	for i := 0; i < fe.m; i++ {
		pkt := make([]byte, FECHeaderSize+maxLen)
		binary.BigEndian.PutUint16(pkt[0:2], fe.blockID)
		pkt[2] = byte(fe.k + i) // parity index
		pkt[3] = byte(fe.k)     // K value
		pkt[4] = byte(fe.m)     // M value
		copy(pkt[FECHeaderSize:], shards[fe.k+i])
		result[i] = pkt
	}

	return result
}

// AdaptRate adjusts K,M based on measured loss rate.
func (fe *FECEncoder) AdaptRate(lossRate float64) error {
	fe.mu.Lock()
	defer fe.mu.Unlock()

	lowThresh := fe.config.FECLowThreshold
	highThresh := fe.config.FECHighThreshold
	if lowThresh == 0 {
		lowThresh = lowLossThreshold
	}
	if highThresh == 0 {
		highThresh = highLossThreshold
	}

	var newK, newM int
	if lossRate < lowThresh {
		newK, newM = fe.config.FECLowK, fe.config.FECLowM
	} else if lossRate < highThresh {
		newK, newM = fe.config.FECMedK, fe.config.FECMedM
	} else {
		newK, newM = fe.config.FECHighK, fe.config.FECHighM
	}
	if newK == 0 {
		newK = fecLowLossK
	}
	if newM == 0 {
		newM = fecLowLossM
	}

	if newK == fe.k && newM == fe.m {
		return nil
	}

	enc, err := reedsolomon.New(newK, newM)
	if err != nil {
		return err
	}

	fe.blockIdx = 0
	fe.currentBlock = make([][]byte, newK)
	fe.k = newK
	fe.m = newM
	fe.encoder = enc

	return nil
}

// NewFECDecoder creates a new FEC decoder.
func NewFECDecoder(blockTimeoutMs, maxBlocks int) *FECDecoder {
	if maxBlocks <= 0 {
		maxBlocks = 256
	}
	return &FECDecoder{
		blocks:         make(map[uint16]*fecBlock),
		maxBlocks:      maxBlocks,
		rsCache:        make(map[uint32]reedsolomon.Encoder),
		blockTimeoutMs: blockTimeoutMs,
	}
}

// getCachedRS returns a cached RS encoder for the given K,M pair.
// Caller must hold fd.mu.
func (fd *FECDecoder) getCachedRS(k, m int) (reedsolomon.Encoder, error) {
	key := uint32(k)<<16 | uint32(m)
	if enc, ok := fd.rsCache[key]; ok {
		return enc, nil
	}
	enc, err := reedsolomon.New(k, m)
	if err != nil {
		return nil, err
	}
	fd.rsCache[key] = enc
	return enc, nil
}

// Decode processes an incoming packet (data or parity) with FEC header.
// Returns:
//   - data: the current data packet with nonce (nil for parity packets)
//   - recovered: any packets recovered via RS reconstruction, each with nonce
//
// Both data and recovered packets have the embedded nonce extracted, so all
// can be fed through the reorder buffer.
func (fd *FECDecoder) Decode(packet []byte) (data *DecodedPacket, recovered []*DecodedPacket) {
	if len(packet) < FECHeaderSize {
		return nil, nil
	}

	blockID := binary.BigEndian.Uint16(packet[0:2])
	index := int(packet[2])
	k := int(packet[3])
	m := int(packet[4])

	if k == 0 || m == 0 || k+m > 128 {
		return nil, nil // invalid or excessively large K+M
	}

	fd.mu.Lock()
	defer fd.mu.Unlock()

	block, exists := fd.blocks[blockID]
	if exists && (k != block.k || m != block.m) {
		return nil, nil // K/M mismatch for existing block — reject
	}
	if !exists {
		// Evict oldest block if at capacity
		if len(fd.blocks) >= fd.maxBlocks {
			var oldestID uint16
			var oldestTime time.Time
			for id, b := range fd.blocks {
				if oldestTime.IsZero() || b.created.Before(oldestTime) {
					oldestID = id
					oldestTime = b.created
				}
			}
			if old, ok := fd.blocks[oldestID]; ok {
				if old.timer != nil {
					old.timer.Stop()
				}
				delete(fd.blocks, oldestID)
				fd.failedCount++
			}
		}

		timeout := fd.blockTimeoutMs
		if timeout <= 0 {
			timeout = fecBlockTimeoutMs
		}

		block = &fecBlock{
			k:       k,
			m:       m,
			shards:  make([][]byte, k+m),
			present: make([]bool, k+m),
			created: time.Now(),
		}
		fd.blocks[blockID] = block

		block.timer = time.AfterFunc(
			time.Duration(timeout)*time.Millisecond,
			func() { fd.blockTimeout(blockID) },
		)
	}

	// Store shard — data packets stored as full packet (FEC header is part of RS),
	// parity packets stored WITHOUT outer FEC header (just the RS shard content)
	if index < len(block.shards) && !block.present[index] {
		var shard []byte
		if index < k {
			// Data shard: full packet [FEC header][nonce][data]
			shard = make([]byte, len(packet))
			copy(shard, packet)
		} else {
			// Parity shard: strip outer FEC header, store RS content only
			content := packet[FECHeaderSize:]
			shard = make([]byte, len(content))
			copy(shard, content)
		}
		block.shards[index] = shard
		block.present[index] = true
		block.received++
		if len(shard) > block.maxLen {
			block.maxLen = len(shard)
		}
	}

	// Data packet (index < K): extract dataSeq + IP payload
	if index < k && len(packet) >= FECPayloadOffset {
		seq := binary.BigEndian.Uint64(packet[FECHeaderSize:FECPayloadOffset])
		payload := make([]byte, len(packet)-FECPayloadOffset)
		copy(payload, packet[FECPayloadOffset:])
		data = &DecodedPacket{Data: payload, DataSeq: seq}
	}

	// Check if we can recover missing data packets
	if block.received >= k {
		recovered = fd.tryRecover(blockID, block)
	}

	return data, recovered
}

// tryRecover attempts to reconstruct missing data packets using FEC.
// Returns recovered packets with their embedded nonces extracted.
func (fd *FECDecoder) tryRecover(blockID uint16, block *fecBlock) []*DecodedPacket {
	var missing []int
	for i := 0; i < block.k; i++ {
		if !block.present[i] {
			missing = append(missing, i)
		}
	}

	if len(missing) == 0 {
		if block.timer != nil {
			block.timer.Stop()
		}
		delete(fd.blocks, blockID)
		return nil
	}

	enc, err := fd.getCachedRS(block.k, block.m)
	if err != nil {
		fd.failedCount++
		return nil
	}

	// Pad present shards to same length — leave missing shards nil
	// (RS library requires nil to identify which shards to reconstruct)
	for i := range block.shards {
		if block.shards[i] != nil && len(block.shards[i]) < block.maxLen {
			padded := make([]byte, block.maxLen)
			copy(padded, block.shards[i])
			block.shards[i] = padded
		}
	}

	err = enc.Reconstruct(block.shards)
	if err != nil {
		fd.failedCount++
		return nil
	}

	// Extract recovered data packets — dataSeq is at bytes 5-13 of each shard
	var recovered []*DecodedPacket
	for _, idx := range missing {
		shard := block.shards[idx]
		if len(shard) >= FECPayloadOffset {
			seq := binary.BigEndian.Uint64(shard[FECHeaderSize:FECPayloadOffset])
			payload := make([]byte, len(shard)-FECPayloadOffset)
			copy(payload, shard[FECPayloadOffset:])
			fd.recoveredCount++
			recovered = append(recovered, &DecodedPacket{Data: payload, DataSeq: seq})
		}
	}

	if block.timer != nil {
		block.timer.Stop()
	}
	delete(fd.blocks, blockID)

	return recovered
}

// blockTimeout cleans up an incomplete FEC block.
func (fd *FECDecoder) blockTimeout(blockID uint16) {
	fd.mu.Lock()
	defer fd.mu.Unlock()

	if _, exists := fd.blocks[blockID]; exists {
		fd.failedCount++
		delete(fd.blocks, blockID)
	}
}

// Stats returns decoder statistics.
func (fd *FECDecoder) Stats() (recovered, failed uint64) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.recoveredCount, fd.failedCount
}

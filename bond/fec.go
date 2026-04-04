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
//   - FEC header (5 bytes) prepended to each packet before encryption
//   - Encoder groups K packets, generates M parity packets
//   - Decoder recovers any M lost packets from remaining K
//
// FEC header format (5 bytes):
//
//	[BlockID (16 bits)][Index (8 bits)][K value (8 bits)][M value (8 bits)]
//	BlockID: which FEC block this packet belongs to
//	Index:   position within block (0..K-1 = data, K..K+M-1 = parity)
//	K:       number of data packets in this block
//	M:       number of parity packets in this block

const (
	FECHeaderSize = 5 // bytes

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

// FECEncoder groups outgoing packets and generates parity packets.
type FECEncoder struct {
	mu sync.Mutex

	k, m    int // current data/parity ratio
	blockID uint16
	encoder reedsolomon.Encoder

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
	blocks map[uint16]*fecBlock

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

// NewFECEncoder creates a new adaptive FEC encoder.
func NewFECEncoder() (*FECEncoder, error) {
	enc, err := reedsolomon.New(fecLowLossK, fecLowLossM)
	if err != nil {
		return nil, err
	}
	fe := &FECEncoder{
		k:            fecLowLossK,
		m:            fecLowLossM,
		encoder:      enc,
		currentBlock: make([][]byte, fecLowLossK),
		lossWindow:   make([]bool, fecLossWindowSize),
	}
	return fe, nil
}

// Encode adds a data packet to the current FEC block.
// Always returns encodedData: the packet with 5-byte FEC header prepended.
// When the block is full (K packets collected), also returns M parity packets.
func (fe *FECEncoder) Encode(data []byte) (encodedData []byte, parityPackets [][]byte) {
	fe.mu.Lock()
	defer fe.mu.Unlock()

	// Prepend FEC header to data packet
	headerData := make([]byte, FECHeaderSize+len(data))
	binary.BigEndian.PutUint16(headerData[0:2], fe.blockID)
	headerData[2] = byte(fe.blockIdx) // index within block
	headerData[3] = byte(fe.k)       // K value
	headerData[4] = byte(fe.m)       // M value
	copy(headerData[FECHeaderSize:], data)

	fe.currentBlock[fe.blockIdx] = headerData
	fe.blockIdx++
	fe.txCount++

	encodedData = headerData

	// Block not full yet
	if fe.blockIdx < fe.k {
		return encodedData, nil
	}

	// Block full — generate parity
	parityPackets = fe.generateParity()

	// Advance to next block
	fe.blockID++
	fe.blockIdx = 0
	fe.currentBlock = make([][]byte, fe.k)

	return encodedData, parityPackets
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
// Call every 500ms.
func (fe *FECEncoder) AdaptRate(lossRate float64) error {
	fe.mu.Lock()
	defer fe.mu.Unlock()

	var newK, newM int
	if lossRate < lowLossThreshold {
		newK, newM = fecLowLossK, fecLowLossM
	} else if lossRate < highLossThreshold {
		newK, newM = fecMedLossK, fecMedLossM
	} else {
		newK, newM = fecHighLossK, fecHighLossM
	}

	if newK == fe.k && newM == fe.m {
		return nil // no change
	}

	enc, err := reedsolomon.New(newK, newM)
	if err != nil {
		return err
	}

	// Flush current block (send what we have without parity)
	fe.blockIdx = 0
	fe.currentBlock = make([][]byte, newK)
	fe.k = newK
	fe.m = newM
	fe.encoder = enc

	return nil
}

// NewFECDecoder creates a new FEC decoder.
func NewFECDecoder() *FECDecoder {
	return &FECDecoder{
		blocks: make(map[uint16]*fecBlock),
	}
}

// Decode processes an incoming packet (data or parity) with FEC header.
// Returns IP packets ready for TUN delivery (FEC header stripped).
//
// For data packets (index < K): returns the payload immediately.
// For parity packets (index >= K): attempts recovery of missing data packets.
// Returns nil if no packets are ready (e.g. parity arrived but can't recover yet).
func (fd *FECDecoder) Decode(packet []byte) [][]byte {
	if len(packet) < FECHeaderSize {
		return nil
	}

	blockID := binary.BigEndian.Uint16(packet[0:2])
	index := int(packet[2])
	k := int(packet[3])
	m := int(packet[4])

	if k == 0 || m == 0 {
		return nil
	}

	fd.mu.Lock()
	defer fd.mu.Unlock()

	block, exists := fd.blocks[blockID]
	if !exists {
		block = &fecBlock{
			k:       k,
			m:       m,
			shards:  make([][]byte, k+m),
			present: make([]bool, k+m),
			created: time.Now(),
		}
		fd.blocks[blockID] = block

		// Timeout to clean up incomplete blocks
		block.timer = time.AfterFunc(
			time.Duration(fecBlockTimeoutMs)*time.Millisecond,
			func() { fd.blockTimeout(blockID) },
		)
	}

	// Store shard
	if index < len(block.shards) && !block.present[index] {
		shard := make([]byte, len(packet))
		copy(shard, packet)
		block.shards[index] = shard
		block.present[index] = true
		block.received++
		if len(shard) > block.maxLen {
			block.maxLen = len(shard)
		}
	}

	var result [][]byte

	// Data packet (index < K): deliver payload immediately
	if index < k {
		payload := make([]byte, len(packet)-FECHeaderSize)
		copy(payload, packet[FECHeaderSize:])
		result = append(result, payload)
	}

	// Check if we can recover missing data packets
	if block.received >= k {
		recovered := fd.tryRecover(blockID, block)
		result = append(result, recovered...)
	}

	return result
}

// tryRecover attempts to reconstruct missing data packets using FEC.
// Returns recovered IP packets (FEC header stripped).
func (fd *FECDecoder) tryRecover(blockID uint16, block *fecBlock) [][]byte {
	// Check which data packets are missing
	var missing []int
	for i := 0; i < block.k; i++ {
		if !block.present[i] {
			missing = append(missing, i)
		}
	}

	if len(missing) == 0 {
		// All data packets already received — clean up
		if block.timer != nil {
			block.timer.Stop()
		}
		delete(fd.blocks, blockID)
		return nil
	}

	// Need Reed-Solomon decoder with exact K,M from sender
	enc, err := reedsolomon.New(block.k, block.m)
	if err != nil {
		fd.failedCount++
		return nil
	}

	// Pad shards to same length
	for i := range block.shards {
		if block.shards[i] == nil {
			block.shards[i] = make([]byte, block.maxLen)
		} else if len(block.shards[i]) < block.maxLen {
			padded := make([]byte, block.maxLen)
			copy(padded, block.shards[i])
			block.shards[i] = padded
		}
	}

	// Attempt reconstruction
	err = enc.Reconstruct(block.shards)
	if err != nil {
		fd.failedCount++
		return nil
	}

	// Extract recovered data packets (strip FEC header)
	var recovered [][]byte
	for _, idx := range missing {
		shard := block.shards[idx]
		if len(shard) > FECHeaderSize {
			payload := make([]byte, len(shard)-FECHeaderSize)
			copy(payload, shard[FECHeaderSize:])
			fd.recoveredCount++
			recovered = append(recovered, payload)
		}
	}

	// Clean up block
	if block.timer != nil {
		block.timer.Stop()
	}
	delete(fd.blocks, blockID)

	return recovered
}

// blockTimeout cleans up an incomplete FEC block.
// Recovery is only done synchronously in Decode — the timer just prevents
// stale blocks from accumulating when packets are permanently lost.
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

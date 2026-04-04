# 007 Bond — Implementation Log

## Base: wireguard-go (MIT License)
- Cloned from: https://github.com/WireGuard/wireguard-go
- Upstream remote: `upstream`
- Branch: `main`

## Change Log

### Phase 1: Multi-Path Send

**Goal**: Send every encrypted packet to ALL configured peer endpoints simultaneously.

#### Change 1.1: Extend Peer endpoint storage

**File**: `device/peer.go`

**Current code** (lines 28-33):
```go
endpoint struct {
    sync.Mutex
    val            conn.Endpoint     // SINGLE endpoint
    clearSrcOnTx   bool
    disableRoaming bool
}
```

**Change**: Add `vals` slice for multiple endpoints while keeping `val` for backward compatibility (primary endpoint).

**Rationale**: The primary endpoint (`val`) is used for handshake and roaming. Additional endpoints (`vals`) are used for multi-path redundancy. All encrypted packets are sent to all endpoints.

#### Change 1.2: Modify SendBuffers to send to all endpoints

**File**: `device/peer.go`

**Current code** (lines 116-145):
```go
func (peer *Peer) SendBuffers(buffers [][]byte) error {
    // ...
    endpoint := peer.endpoint.val        // Gets single endpoint
    err := peer.device.net.bind.Send(buffers, endpoint)  // Sends once
    // ...
}
```

**Change**: Loop over all endpoints (primary + additional) and send to each.

**Rationale**: This is the core multi-path change. Each encrypted packet is sent via all paths. The receiver gets duplicates and uses the first arrival, discarding the rest via the replay filter.

#### Change 1.3: Extend UAPI to accept multiple endpoints

**File**: `device/uapi.go`

**Current**: Single `endpoint=<ip>:<port>` per peer.

**Change**: Accept `bond_endpoint=<ip>:<port>` for additional endpoints. Keep `endpoint=` for the primary (backward compatible).

**Rationale**: Allows configuration via `wg set` or config files. Primary endpoint handles handshake/roaming. Bond endpoints are send-only redundancy paths.

---

### Phase 2: FEC (Forward Error Correction)

**Goal**: Add adaptive Reed-Solomon FEC to recover from packet loss without retransmission.

#### Change 2.1: FEC encoder (new file)

**File**: `bond/fec.go`

**Design**:
- Groups outgoing packets into blocks of K packets
- Generates M parity packets using Reed-Solomon
- Parity packets sent alongside data packets
- Uses `klauspost/reedsolomon` Go library
- Adaptive: adjusts K,M based on measured loss rate

**Packet format for FEC**:
```
[WireGuard transport header][FEC header (4 bytes)][encrypted payload]

FEC header:
  - Block ID (16 bits): which FEC block this packet belongs to
  - Index (8 bits): position within block (0..K-1 = data, K..K+M-1 = parity)
  - K value (8 bits): number of data packets in this block
```

**Insertion point**: Between TUN read and WireGuard encryption in the send path.

#### Change 2.2: FEC decoder (new file)

**File**: `bond/fec.go` (same file, decoder section)

**Design**:
- Collects packets by Block ID
- When K packets received (any K of K+M), decode immediately
- If all K data packets received, no decoding needed (just deliver)
- If <K received after timeout, deliver what we have (gap)
- Timeout: configurable, default 50ms

#### Change 2.3: Adaptive FEC ratio

**File**: `bond/fec.go`

**Design**:
- Measure packet loss rate per path over sliding window (last 100 packets)
- Every 500ms, adjust K,M:
  - Loss <1%: K=16, M=2 (12.5% overhead)
  - Loss 1-5%: K=12, M=4 (25% overhead)
  - Loss >5%: K=8, M=6 (42% overhead)

---

### Phase 3: Reorder Buffer

**Goal**: Deliver packets to TUN in sequence order despite multi-path arrival timing differences.

#### Change 3.1: Reorder buffer (new file)

**File**: `bond/reorder.go`

**Design**:
- Ring buffer with pre-allocated slots
- Uses WireGuard nonce (64-bit counter) as sequence number
- Per-path timeout based on measured path latency
- Adaptive window size: max path latency + 20ms margin

**Data structure**:
```go
type ReorderBuffer struct {
    slots      [bufferSize]*BufferedPacket  // ring buffer
    nextExpect uint64                        // next expected sequence number
    maxWindow  time.Duration                // max wait before delivering
    pathRTT    map[int]time.Duration        // measured RTT per path
}
```

**Delivery rules**:
1. If packet with `nextExpect` sequence arrives → deliver immediately, advance
2. If later sequence arrives → buffer, wait for earlier ones
3. If timeout expires for a gap → skip missing, deliver buffered packets
4. If FEC recovers the missing packet → insert and deliver in order

#### Change 3.2: Path latency tracking

**File**: `bond/path.go`

**Design**:
- Track per-path RTT from keepalive round-trips
- Exponential moving average: `rtt = 0.8 * rtt + 0.2 * sample`
- Feed path RTT into reorder buffer timeout calculation
- Report path stats via management API

---

### Phase 4: ARQ (Automatic Repeat Request)

**Goal**: Request retransmission of packets that FEC cannot recover.

#### Change 4.1: NACK generation

**File**: `bond/arq.go`

**Design**:
- When reorder buffer gap exceeds FEC recovery → generate NACK
- NACK contains: sequence numbers of missing packets
- Send NACK via control channel (separate from data)
- Rate-limited: max 1 NACK per 10ms

#### Change 4.2: Retransmission

**File**: `bond/arq.go`

**Design**:
- Sender maintains retransmit buffer (last N packets)
- On NACK receipt, retransmit requested packets
- Retransmit on all paths (redundant retransmit)
- Buffer size: 200 packets (4 seconds at 50pps)

---

## File Map

| File | Status | Description |
|------|--------|-------------|
| `device/peer.go` | TO MODIFY | Multi-endpoint storage + SendBuffers loop |
| `device/uapi.go` | TO MODIFY | bond_endpoint= config |
| `device/receive.go` | TO MODIFY | Path tracking on receive |
| `bond/fec.go` | NEW | Reed-Solomon FEC encoder/decoder |
| `bond/reorder.go` | NEW | Adaptive reorder buffer |
| `bond/arq.go` | NEW | NACK-based retransmission |
| `bond/path.go` | NEW | Path health tracking + RTT |
| `bond/bond.go` | NEW | Bond manager (ties components together) |
| `docs/IMPLEMENTATION.md` | NEW | This file |

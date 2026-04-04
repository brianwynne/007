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

**Change**: Added `bondEndpoints []conn.Endpoint` slice alongside the primary `val` endpoint.

**Rationale**: The primary endpoint (`val`) is used for handshake and roaming. Additional endpoints (`bondEndpoints`) are used for multi-path redundancy. All encrypted packets are sent to all endpoints.

#### Change 1.2: Modify SendBuffers to send to all endpoints

**File**: `device/peer.go`

**Change**: `SendBuffers()` loops over primary + all bond endpoints, sending to each.

**Rationale**: Core multi-path change. Each encrypted packet is sent via all paths. The receiver gets duplicates and uses the first arrival, discarding the rest via the replay filter.

#### Change 1.3: Extend UAPI to accept multiple endpoints

**File**: `device/uapi.go`

**Change**: Accept `bond_endpoint=<ip>:<port>` for additional endpoints, `clear_bond_endpoints` to remove them. Standard `endpoint=` unchanged.

**Rationale**: Backward compatible — standard WireGuard config still works. Bond endpoints are send-only redundancy paths.

---

### Phase 2: FEC (Forward Error Correction)

**Goal**: Add adaptive Reed-Solomon FEC to recover from packet loss without retransmission.

#### Change 2.1: FEC encoder/decoder

**File**: `bond/fec.go`

**Design**:
- Groups outgoing packets into blocks of K packets
- Generates M parity packets using Reed-Solomon (`klauspost/reedsolomon`)
- Parity packets sent alongside data packets
- Adaptive: adjusts K,M based on measured loss rate every 500ms

**Packet format for FEC (5 bytes)**:
```
[FEC header][IP packet data]  ← this is the cleartext before WireGuard encryption

FEC header:
  - Block ID (16 bits): which FEC block this packet belongs to
  - Index (8 bits): position within block (0..K-1 = data, K..K+M-1 = parity)
  - K value (8 bits): number of data packets in this block
  - M value (8 bits): number of parity packets in this block
```

**Adaptive FEC presets** (adjusted every 500ms based on measured loss):
| Loss rate | K | M | Overhead |
|-----------|---|---|----------|
| <1%       | 16| 2 | 12.5%    |
| 1-5%      | 12| 4 | 25%      |
| >5%       | 8 | 6 | 42%      |

**Decoder**:
- Collects packets by Block ID into shard arrays
- Data packets (index < K): delivered immediately, also stored for potential recovery
- Parity packets (index >= K): stored, triggers RS recovery if K shards present
- Recovery is synchronous — happens on each shard arrival
- Block timeout (50ms): cleans up incomplete blocks, counts as failed recovery
- Keepalive packets (empty payload) bypass FEC entirely

#### Change 2.2: Bond manager

**File**: `bond/bond.go`

**Design**:
- `ProcessOutbound(packet, nonce) → [][]byte`: FEC encode, returns data + parity packets
- `ProcessInbound(packet, nonce, pathID) → [][]byte`: FEC decode, returns clean IP packets
- Adaptive FEC ratio goroutine (500ms interval)
- Stats API for monitoring

#### Change 2.3: Pipeline integration

**Files**: `device/send.go`, `device/receive.go`, `device/device.go`

**Send path** (`SendStagedPackets`, after nonce assignment):
1. Skip keepalive packets (empty) — no FEC encoding
2. For each data packet, call `ProcessOutbound` which prepends 5-byte FEC header
3. When FEC block fills (K packets), parity packets are generated
4. Each parity packet gets a new `QueueOutboundElement` with its own nonce from `keypair.sendNonce`
5. Parity elements appended to container — go through normal encryption and multi-path send

**Receive path** (`RoutineSequentialReceiver`, after replay filter):
1. Keepalive detection occurs before bond processing (standard WireGuard check)
2. Call `ProcessInbound` which strips FEC header and returns clean IP packets
3. Data packets: payload delivered immediately
4. Parity packets: trigger RS recovery of missing data if enough shards present
5. All returned packets undergo IP validation (IPv4/IPv6 header + allowed IPs)
6. FEC-recovered packets get new message buffers, tracked and freed after TUN write

**Device** (`device/device.go`):
- `bondManager` interface with `ProcessOutbound(peerID, ...)`/`ProcessInbound(peerID, ...)` methods
- `SetBondManager()` — must be called before device is brought up
- Interface type avoids circular imports between `device` and `bond` packages
- `nextBondPeerID` atomic counter assigns unique IDs to peers
- Each peer carries `bondPeerID` (assigned in `NewPeer`) for per-peer FEC isolation

---

### Phase 3: Reorder Buffer

**Goal**: Deliver packets to TUN in sequence order despite multi-path arrival timing differences.

**File**: `bond/reorder.go`

**Design**:
- Ring buffer (64 slots, zero-alloc after init)
- Synchronous API: `Insert()` returns packets ready for delivery
- Uses WireGuard nonce (64-bit counter) as sequence number
- Per-path timeout based on measured RTT: `RTT + 2*RTTVar + 10ms`
- Adaptive window: 80ms default, 20ms min, 200ms max
- Gap handling: gap timeout checked on each Insert call + periodic Flush
- Per-peer: each peer gets its own reorder buffer (nonces are per-peer)

**Integration with FEC**:
- FEC `Decode()` returns `(data, recovered)` separately
- Data packets → reorder buffer (have known WireGuard nonce)
- FEC-recovered packets → delivered immediately, bypass reorder (no nonce available)
- Periodic Flush (10ms) advances `nextExpect` during idle periods

---

### Phase 4: ARQ (Automatic Repeat Request)

**Goal**: Request retransmission of packets that FEC cannot recover.

**Status**: Not yet implemented.

---

## File Map

| File | Status | Description |
|------|--------|-------------|
| `device/peer.go` | MODIFIED | Multi-endpoint storage + SendBuffers loop |
| `device/uapi.go` | MODIFIED | bond_endpoint= config |
| `device/device.go` | MODIFIED | bondManager interface + SetBondManager() |
| `device/send.go` | MODIFIED | FEC encode in SendStagedPackets |
| `device/receive.go` | MODIFIED | FEC decode in RoutineSequentialReceiver |
| `bond/fec.go` | IMPLEMENTED | Reed-Solomon FEC encoder/decoder (5-byte header) |
| `bond/reorder.go` | IMPLEMENTED | Adaptive reorder buffer (not yet wired) |
| `bond/bond.go` | IMPLEMENTED | Bond manager — ProcessOutbound/ProcessInbound API |
| `bond/arq.go` | NOT STARTED | NACK-based retransmission |
| `bond/path.go` | NOT STARTED | Path health tracking + RTT |
| `docs/IMPLEMENTATION.md` | IMPLEMENTED | This file |

## What's Connected
- [x] Multi-path send (peer.go SendBuffers sends to all endpoints)
- [x] UAPI bond_endpoint config (uapi.go)
- [x] FEC encoder/decoder (bond/fec.go, 5-byte header with K and M)
- [x] Bond manager API (bond/bond.go)
- [x] ProcessOutbound wired into device/send.go (keepalive-safe)
- [x] ProcessInbound wired into device/receive.go (with IP validation + buffer management)
- [x] Reorder buffer wired into receive pipeline (per-peer, synchronous API)

## What's NOT Connected Yet
- [ ] Interface manager (bind sockets to physical interfaces)
- [ ] ARQ (NACK retransmission)
- [ ] Path health tracking
- [ ] Management API

## Design Constraints
- **Per-peer FEC state**: Each peer gets its own FEC encoder/decoder via `peerID`. Multiple devices (e.g. field SIP Reporters) connecting to a single 007 server each have isolated FEC blocks — packets from different peers are never mixed.
- **Both ends must run 007**: FEC header prepended to all data packets makes them incompatible with standard WireGuard receivers. No negotiation/fallback.
- **MTU consideration**: FEC adds 5 bytes to data packets and up to 10 bytes to parity packets. With default WireGuard MTU of 1420, parity packets are at the Ethernet fragmentation boundary. Recommend MTU 1412 when FEC is enabled.
- **Traffic separation**: Only traffic routed to the WireGuard TUN interface enters the bond tunnel. Other system traffic uses normal interfaces. Application binds to tunnel IP or uses policy routing.

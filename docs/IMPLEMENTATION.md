# 007 Bond — Implementation Details

## Base

Fork of [wireguard-go](https://github.com/WireGuard/wireguard-go) (MIT License). The WireGuard encryption, handshake, and key management are unmodified. 007 Bond adds a bond layer between the TUN interface and the crypto layer.

## Architecture

```
SEND PATH:
  App → TUN (bond0) → FEC encode → WireGuard encrypt → multi-path send (all interfaces)
  Control packets (probes/NACKs/preset) bypass FEC — sent raw

RECEIVE PATH:
  multi-path receive → WireGuard decrypt → control packet check →
    FEC decode → ARQ gap detection (races FEC) → jitter buffer → TUN
```

## Recovery Chain

Recovery mechanisms fire in this order, each independent:

1. **Multi-path diversity** — same packet sent on all interfaces. First copy wins. Independent per-path loss means compound loss probability is very low (30% per path = 9% both).

2. **Sliding-window FEC (XOR, W=5)** — each data packet generates one repair packet covering the last 5 data packets. Single loss recovery in 20ms. Overlapping windows recover bursts up to W-1=4 consecutive losses.

3. **ARQ (NACK-based)** — fires for every sequence gap immediately, racing FEC in parallel. If FEC recovers the packet, the retransmit is a harmless duplicate. If FEC fails, ARQ is already in flight. Deadline-checked against jitter buffer playout time.

4. **Jitter buffer** — per-packet playout deadline. Each packet gets `deadline = insertTime + bufferDepth`. Rate-independent — works for any traffic pattern.

## Presets

| Preset | LatencyBudgetMs | FEC K | FEC M | Jitter Buffer | Use Case |
|--------|----------------|-------|-------|---------------|----------|
| broadcast | 40 | 2 | 2 | 20ms (1 slot) | Live contribution |
| studio | 80 | 2 | 2 | 60ms (3 slots) | Studio links |
| field | 200 | 2 | 4 | 180ms (9 slots) | WiFi + cellular |

Jitter buffer depth = LatencyBudgetMs - (K-1) * PacketIntervalMs.

Runtime preset changes via `POST /api/preset` or `007-bond preset <name>`. Client sends preset control packet (type=5) through the tunnel — server changes only that peer's jitter buffer. Each peer has an independent jitter buffer on the server.

## Wire Formats

### Sliding FEC Data Packet
```
[Type=0x01][Flags=0x00][DataSeq (8 bytes)][IP payload]
```

### Sliding FEC Repair Packet
```
[Type=0x02][Flags=0x00][RepairSeq (8)][WindowStart (8)][WindowSize (1)][XOR data]
```

### Block FEC (Reed-Solomon)
```
[BlockID (2)][Idx (1)][K (1)][M (1)][DataSeq (8)][IP payload or parity shard]
```

### Control Packets
All control packets start with `BlockID=0xFFFF` to distinguish from FEC data:
```
[0xFF][0xFF][Type (1)][K=0][M=0][payload...]
```

| Type | Format | Purpose |
|------|--------|---------|
| 1 (NACK) | `[count (2)][seq1 (8)][seq2 (8)]...` | Request retransmission |
| 2 (Probe) | `[pathID (4)][timestamp (8)]...` | RTT measurement |
| 3 (Echo) | `[pathID (4)][timestamp (8)]...` | RTT response |
| 4 (Retransmit) | `[dataSeq (8)][IP payload]` | Retransmitted packet |
| 5 (Preset) | `[preset name bytes]` | Signal preset change |

Control packets bypass FEC encoding (not assigned dataSeq, not counted in FEC blocks).

## FEC Decoder Gap Detection

The sliding FEC decoder tracks the highest data sequence seen (`nextExpected`). When a data packet arrives with `seq > nextExpected`, all sequences between are reported as missing — regardless of whether FEC has already recovered them. This ensures ARQ fires for every gap unconditionally, racing FEC.

```go
// In SlidingFECDecoder.Decode():
if seq > d.nextExpected {
    for s := d.nextExpected; s < seq; s++ {
        missing = append(missing, s)  // no d.received check — race FEC
    }
}
```

## ARQ Flow

1. Receiver detects gap (FEC decoder reports missing sequences)
2. NACKs fired immediately via `nackTracker` → `sendFunc`
3. FEC recovery runs in parallel (may fill the gap)
4. Sender receives NACK → looks up original packet in `retransmitBuffer` → sends retransmit
5. Retransmit arrives as control packet (type=4) → inserted into jitter buffer with `sourceARQ`
6. If FEC already recovered it, retransmit is a duplicate (harmlessly discarded)

Deadline check: before sending a NACK, verify at least one path has RTT < remaining time until playout deadline. If not, skip (`arq_deadline_skip` counter).

## Per-Path Health

Each path tracks:
- **RTT** via probe/echo packets (EWMA)
- **Jitter** from RTT variance
- **Loss** via sliding window
- **Burst loss** count and max burst length
- **State machine**: healthy → degraded → unstable → failed → recovering

## Bond Path Configuration

Via WireGuard UAPI:
```bash
# Add bond paths
wg set bond0 peer <pubkey> \
  bond_endpoint=server:51820@eth0_ip \
  bond_endpoint=server:51820@wlan0_ip

# Clear all bond paths
printf "set=1\npublic_key=<hex>\nclear_bond_endpoints=true\n" | \
  nc -U /var/run/wireguard/bond0.sock -w 1
```

The client's path monitor timer (`007-bond-paths.timer`) re-scans interfaces every 30 seconds. Only interfaces with a route to the server are added as bond paths.

## File Map

### Bond Package (`bond/`)
| File | Purpose |
|------|---------|
| `bond.go` | Manager, ProcessInbound/Outbound, presets, SetPreset/SetPeerPreset |
| `fec_sliding.go` | XOR sliding-window FEC encoder/decoder with gap detection |
| `fec.go` | Reed-Solomon block FEC encoder/decoder |
| `jitter.go` | Per-packet-deadline jitter buffer with SetDepth |
| `arq.go` | NACK tracker, retransmit buffer, control packet builders |
| `path.go` | Per-path health tracking, state machine, probe/echo |
| `reorder.go` | Legacy reorder buffer (used when jitter disabled) |
| `api.go` | REST management API |

### Modified WireGuard Files (`device/`)
| File | Changes |
|------|---------|
| `device.go` | BondManager interface, SetBondManager, bondFECOverhead MTU |
| `peer.go` | BondPath struct, AddBondPath, bondPeerID, sendFunc/tunWriter callbacks |
| `send.go` | FEC encode block, control packet skip, parity element creation |
| `receive.go` | ProcessInbound call, IP validation for recovered packets, bondExtraBufs |
| `uapi.go` | `bond_endpoint` and `clear_bond_endpoints` UAPI commands |

### Deployment (`deploy/`)
| File | Purpose |
|------|---------|
| `install-007-server.sh` | Server installer (FHS, systemd, enrollment, firewall) |
| `install-007-client.sh` | Client installer (enrollment or manual keys, path monitor) |
| `007-cli.sh` | Management CLI (`/usr/local/bin/007-bond`) |
| `enroll-server.sh` | Python enrollment HTTP service |

### Tests (`tests/`)
| File | Purpose |
|------|---------|
| `007-impairment-suite.sh` | 32-test tc/netem + iptables impairment suite |
| `007-sliding-server.sh` | Self-contained sliding FEC server test |
| `007-sliding-client.sh` | Self-contained sliding FEC client test |

## API Response Examples

### GET /api/stats
```json
{
  "system_state": "healthy",
  "tx_packets": 1957,
  "rx_packets": 5765,
  "fec_recovered": 32,
  "nacks_sent": 27,
  "arq_received": 36,
  "jitter_delivered": 1779,
  "jitter_late": 14,
  "jitter_fec_fills": 30,
  "jitter_arq_fills": 2,
  "paths": [
    {"path_id": 1603322990, "state": "healthy", "rtt_ms": 0.61, "loss": 0}
  ]
}
```

### GET /api/preset
```json
{
  "preset": "broadcast",
  "latency_budget_ms": 40
}
```

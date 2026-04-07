# 007 Bond — Multi-Path Network Bonding

007 Bond is a multi-path network bonding solution built on [wireguard-go](https://git.zx2c4.com/wireguard-go). It sends every packet simultaneously across all configured network interfaces (ethernet, WiFi, cellular) and uses FEC, ARQ, and a jitter buffer to deliver reliable, low-latency connectivity over unreliable links.

Designed for broadcast field contribution where dropping audio is not an option.

## How It Works

```
APP (e.g. SIP Reporter)
        |
   ┌────┴────┐  TUN interface (bond0)
   │   007   │  FEC encode → encrypt → multi-path send
   └──┬──┬──┬┘
      |  |  |
   eth0 wlan0 wwan0  ── all paths simultaneously ──  server
```

- Every encrypted packet is sent on ALL configured paths
- Receiver discards duplicates (WireGuard replay filter)
- Sliding-window FEC (XOR) recovers single losses in 20ms
- ARQ fires NACKs for every gap, racing FEC in parallel
- Jitter buffer holds packets until playout deadline
- Per-path health tracking (RTT, loss, jitter) drives adaptive timeouts

## Install

### Server

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-server.sh | sudo -E bash
```

### Client (via enrollment)

```bash
# On server — generate a one-time enrollment token:
sudo 007-bond enroll-token

# On client — install with token (generates keys locally, exchanges via API):
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh | \
  sudo ENROLL_URL=http://<server>:8017 ENROLL_TOKEN=<token> bash
```

### Client (manual keys)

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh | \
  sudo SERVER_IP=<ip> SERVER_PUB=<key> CLIENT_KEY=<key> bash
```

## Presets

| Preset | Latency Budget | Jitter Buffer | FEC | Use Case |
|--------|---------------|---------------|-----|----------|
| broadcast | 40ms | 20ms (1 slot) | K=2 M=2 | Live broadcast, low latency critical |
| studio | 80ms | 60ms (3 slots) | K=2 M=2 | Studio-to-studio, managed networks |
| field | 200ms | 180ms (9 slots) | K=2 M=4 | Field contribution, WiFi + cellular |

Change preset at runtime (no restart — signals peers automatically):

```bash
sudo 007-bond preset broadcast    # set locally + signal all peers
sudo 007-bond preset field 10.7.0.3  # set on a specific peer (remote help)
```

## Management CLI

```bash
sudo 007-bond status          # service, interface, peers, preset
sudo 007-bond stats           # FEC, ARQ, jitter buffer statistics
sudo 007-bond paths           # per-path health (RTT, loss, jitter)
sudo 007-bond preset [name]   # show or set latency preset
sudo 007-bond logs            # tail service logs
sudo 007-bond start|stop|restart
sudo 007-bond enroll-token    # generate one-time client enrollment token
sudo 007-bond list-tokens     # show pending enrollment tokens
sudo 007-bond upgrade         # upgrade to latest release
sudo 007-bond version         # show installed version
sudo 007-bond uninstall       # remove (preserves config/data)
```

## Management API

```bash
curl http://127.0.0.1:8007/api/stats    # FEC, ARQ, jitter stats + paths
curl http://127.0.0.1:8007/api/paths    # per-path RTT, jitter, loss
curl http://127.0.0.1:8007/api/config   # current configuration
curl http://127.0.0.1:8007/api/preset   # current preset and latency budget
curl http://127.0.0.1:8007/api/health   # health check
```

POST `/api/preset` with `{"preset":"broadcast"}` to change at runtime.

## Recovery Chain

```
1. Multi-path diversity — same packet on all interfaces (first line of defence)
2. Sliding FEC (XOR, W=5) — recovers single losses within 20ms window
3. ARQ — NACKs fire for every gap, racing FEC in parallel
4. Jitter buffer — holds packets for playout deadline, delivers in order
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BOND_PRESET` | `field` | Latency preset: `broadcast`, `studio`, `field` |
| `BOND_FEC_MODE` | `block` | FEC strategy: `block` (Reed-Solomon) or `sliding` (XOR) |
| `BOND_FEC` | enabled | Set to `0` to disable FEC |
| `BOND_JITTER` | enabled | Set to `0` to disable jitter buffer |
| `BOND_REORDER` | enabled | Set to `0` to disable reorder buffer |
| `BOND_API` | `127.0.0.1:8007` | Management API listen address |
| `BOND_API_KEY` | empty | Optional API authentication key |
| `LOG_LEVEL` | `error` | `verbose`, `error`, or `silent` |

## Deployment Layout

```
/opt/007/           — binary, helper scripts
/etc/007/           — config (.env), WireGuard keys, enrollment tokens
/var/lib/007/       — persistent data
/var/log/007/       — logs
```

Runs as dedicated `bond007` system user with Linux capabilities (`CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_NET_BIND_SERVICE`) — not root.

Systemd services:
- `007-bond` — main bonding service
- `007-bond-enroll` — enrollment API (server only, port 8017)
- `007-bond-paths.timer` — auto-detects interfaces every 30s (client only)

## Testing

```bash
# Run impairment test suite (32 tests across 10 sections)
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/007-impairment-suite.sh | sudo bash

# Run unit tests
go test ./bond/ -v
```

## Building from Source

Requires Go 1.23+.

```bash
go build -o 007 .                                    # Linux (native)
GOOS=linux GOARCH=arm64 go build -o 007-arm64 .      # Raspberry Pi 4
GOOS=linux GOARCH=arm go build -o 007-arm .           # Raspberry Pi 3
```

## Documentation

- [Installation and Testing Guide](docs/INSTALL.md)
- [Implementation Details](docs/IMPLEMENTATION.md)

## Based On

Fork of [wireguard-go](https://git.zx2c4.com/wireguard-go) (MIT License, Copyright 2017-2025 WireGuard LLC).

007 Bond adds the `bond/` package and modifies `device/send.go`, `device/receive.go`, `device/device.go`, `device/peer.go`, and `device/uapi.go` for pipeline integration. The WireGuard encryption, handshake, and key management are unmodified.

## License

MIT License. See [LICENSE](LICENSE).

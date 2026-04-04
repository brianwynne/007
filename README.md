# 007 Bond — Multi-Path Network Bonding

007 Bond is a multi-path network bonding solution built on [wireguard-go](https://git.zx2c4.com/wireguard-go). It sends every packet simultaneously across all configured network interfaces (ethernet, WiFi, cellular) and uses FEC, reordering, and ARQ to deliver reliable, low-latency connectivity over unreliable links.

Designed for broadcast field contribution where dropping audio is not an option.

## How It Works

```
APP (e.g. SIP codec)
        |
   ┌────┴────┐ TUN interface (bond0)
   │   007   │ FEC encode → encrypt → multi-path send
   └──┬───┬──┘
      |   |
   eth0  wlan0  ──── all paths simultaneously ────  server
```

- Every encrypted packet is sent on ALL configured paths
- Receiver discards duplicates (WireGuard replay filter)
- FEC (Reed-Solomon) recovers lost packets without retransmission
- Reorder buffer delivers packets in sequence despite path latency differences
- ARQ requests retransmission for anything FEC can't recover
- Per-path health tracking (RTT, loss, jitter) drives adaptive timeouts

## Quick Start

```bash
# Build
go build -o 007 .

# Server
sudo ip tuntap add dev bond0 mode tun
sudo ./007 -f bond0 &
sudo wg set bond0 listen-port 51820 private-key ./server.key
sudo wg set bond0 peer <CLIENT_PUBKEY> allowed-ips 10.7.0.2/32
sudo ip addr add 10.7.0.1/24 dev bond0
sudo ip link set bond0 up

# Client
sudo ip tuntap add dev bond0 mode tun
sudo ./007 -f bond0 &
sudo wg set bond0 private-key ./client.key \
  peer <SERVER_PUBKEY> \
    endpoint server:51820 \
    allowed-ips 10.7.0.0/24 \
    persistent-keepalive 25 \
    bond_endpoint=server:51820@192.168.1.100 \
    bond_endpoint=server:51820@10.0.0.50
sudo ip addr add 10.7.0.2/24 dev bond0
sudo ip link set bond0 up
```

`bond_endpoint=dest@localip` — send to `dest` via the interface that owns `localip`.

## Features

| Feature | Description |
|---------|-------------|
| Multi-path send | Every packet sent on all configured endpoints simultaneously |
| FEC | Adaptive Reed-Solomon (K=8-16, M=2-6), adjusts to measured loss |
| Reorder buffer | Adaptive window (20-200ms), per-path timeout based on measured RTT |
| ARQ | NACK-based retransmission for gaps FEC can't recover |
| Path health | Probe/echo RTT, per-path loss and jitter tracking |
| Per-peer isolation | Multiple devices connect to one server without interference |
| Interface binding | Per-path UDP sockets bound to specific local IPs |
| Management API | REST API on :8007 for stats, path health, config |
| WireGuard compatible | Standard `wg` tool for key management and peer config |
| Auto MTU | Effective MTU reduced automatically for FEC overhead |

## Management API

```bash
curl http://127.0.0.1:8007/api/stats   # FEC, reorder, ARQ stats + paths
curl http://127.0.0.1:8007/api/paths   # per-path RTT, jitter, loss
curl http://127.0.0.1:8007/api/config  # current configuration
curl http://127.0.0.1:8007/api/health  # health check
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BOND_FEC` | enabled | Set to `0` to disable FEC |
| `BOND_REORDER` | enabled | Set to `0` to disable reorder buffer |
| `BOND_API` | `127.0.0.1:8007` | Management API listen address |
| `LOG_LEVEL` | `error` | Set to `verbose` for debug logging |

## Documentation

- [Installation and Testing Guide](docs/INSTALL.md) — full setup, 6 test scenarios, troubleshooting
- [Implementation Log](docs/IMPLEMENTATION.md) — architecture, design decisions, wire formats

## Building

Requires Go 1.23+.

```bash
go build -o 007 .                                    # Linux (native)
GOOS=linux GOARCH=arm64 go build -o 007-arm64 .      # Raspberry Pi 4
GOOS=linux GOARCH=arm go build -o 007-arm .           # Raspberry Pi 3
go test ./bond/ -v                                    # Run tests
```

## Based On

Fork of [wireguard-go](https://git.zx2c4.com/wireguard-go) (MIT License, Copyright 2017-2025 WireGuard LLC).

007 Bond adds the `bond/` package and modifies `device/send.go`, `device/receive.go`, `device/device.go`, `device/peer.go`, and `device/uapi.go` for pipeline integration. The WireGuard encryption, handshake, and key management are unmodified.

## License

MIT License. See [LICENSE](LICENSE).

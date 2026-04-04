# 007 Bond — Installation and Testing Guide

## Prerequisites

- **Go 1.23+**: https://go.dev/dl/
- **Linux** (kernel 3.10+) for TUN device support
- **Root access** for creating TUN interfaces
- **Two or more network interfaces** for actual multi-path testing

```bash
# Verify Go version
go version    # must be 1.23 or later
```

## Build

```bash
cd /mnt/c/Users/tighm/007

# Run tests first
go test ./bond/ -v

# Build the binary
go build -o 007 .

# Verify
./007 --version
```

Cross-compile for a target device (e.g., Raspberry Pi):
```bash
GOOS=linux GOARCH=arm64 go build -o 007-arm64 .
GOOS=linux GOARCH=arm   go build -o 007-arm .
```

## Network Setup

### Architecture

```
┌─────────────────────────┐         ┌─────────────────────────┐
│  CLIENT (field device)  │         │  SERVER (data centre)   │
│                         │         │                         │
│  ┌─────────┐            │         │            ┌─────────┐  │
│  │ bond0   │ tunnel IP  │         │  tunnel IP │ bond0   │  │
│  │10.7.0.2 │            │         │            │10.7.0.1 │  │
│  └────┬────┘            │         │            └────┬────┘  │
│       │ 007             │         │             007 │       │
│  ┌────┴────────────┐    │         │    ┌────────────┴────┐  │
│  │ eth0   │ wlan0  │    │         │    │ eth0 (public)   │  │
│  │.1.100  │ .0.50  │    │         │    │ 203.0.113.1     │  │
│  └────┬───┴───┬────┘    │         │    └────────┬────────┘  │
└───────┼───────┼─────────┘         └─────────────┼───────────┘
        │       │                                 │
    ethernet   WiFi ─────────────────────────── internet
```

### Server Setup

The server is the fixed endpoint that all field devices connect to.

```bash
# 1. Generate keys
wg genkey | tee server_private.key | wg pubkey > server_public.key
wg genkey | tee client_private.key | wg pubkey > client_public.key

# 2. Create TUN interface
sudo ip tuntap add dev bond0 mode tun

# 3. Start 007
sudo ./007 -f bond0 &

# 4. Configure WireGuard identity
sudo wg set bond0 \
  listen-port 51820 \
  private-key ./server_private.key

# 5. Add client peer
sudo wg set bond0 \
  peer $(cat client_public.key) \
  allowed-ips 10.7.0.2/32

# 6. Assign tunnel IP and bring up
sudo ip addr add 10.7.0.1/24 dev bond0
sudo ip link set bond0 up

# 7. Enable IP forwarding (if routing traffic)
sudo sysctl -w net.ipv4.ip_forward=1
```

### Client Setup

The client is the field device with multiple network interfaces.

```bash
# 1. Create TUN interface
sudo ip tuntap add dev bond0 mode tun

# 2. Start 007
sudo ./007 -f bond0 &

# 3. Configure WireGuard identity + server endpoint
sudo wg set bond0 \
  private-key ./client_private.key \
  peer $(cat server_public.key) \
    endpoint 203.0.113.1:51820 \
    allowed-ips 10.7.0.0/24 \
    persistent-keepalive 25 \
    bond_endpoint=203.0.113.1:51820@192.168.1.100 \
    bond_endpoint=203.0.113.1:51820@10.0.0.50

# 4. Assign tunnel IP and bring up
sudo ip addr add 10.7.0.2/24 dev bond0
sudo ip link set bond0 up
```

**Key line**: `bond_endpoint=203.0.113.1:51820@192.168.1.100` means "send to server:51820 via the interface that owns 192.168.1.100 (ethernet)". The `@` binds the send socket to that local IP.

### Verify Connectivity

```bash
# From client
ping 10.7.0.1

# From server
ping 10.7.0.2

# Check WireGuard status
sudo wg show bond0

# Check bond stats
curl http://127.0.0.1:8007/api/stats | jq .
curl http://127.0.0.1:8007/api/paths | jq .
```

## Testing Scenarios

### Test 1: Basic Tunnel

Verify the tunnel works before adding bond paths.

```bash
# Client (no bond endpoints yet):
sudo wg set bond0 peer $(cat server_public.key) \
  endpoint 203.0.113.1:51820 \
  allowed-ips 10.7.0.0/24

ping -c 10 10.7.0.1
# Should see ~0% loss, RTT matching your primary path
```

### Test 2: Multi-Path Redundancy

Add bond endpoints and verify traffic flows on all paths.

```bash
# Add bond paths
sudo wg set bond0 peer $(cat server_public.key) \
  bond_endpoint=203.0.113.1:51820@192.168.1.100 \
  bond_endpoint=203.0.113.1:51820@10.0.0.50

# Monitor traffic on each interface (in separate terminals)
sudo tcpdump -i eth0 udp port 51820 -c 20
sudo tcpdump -i wlan0 udp port 51820 -c 20

# Generate traffic
ping -c 20 10.7.0.1

# Both tcpdump sessions should show packets — multi-path is working
```

### Test 3: Path Failure Recovery

Simulate a path failure and verify seamless failover.

```bash
# Start a continuous ping
ping 10.7.0.1

# In another terminal, disable WiFi
sudo ip link set wlan0 down

# Ping should continue without interruption (ethernet still active)

# Re-enable WiFi
sudo ip link set wlan0 up

# Check stats — should show brief gap on WiFi path
curl http://127.0.0.1:8007/api/paths | jq .
```

### Test 4: FEC Recovery

Simulate packet loss and verify FEC recovers without retransmission.

```bash
# Add artificial packet loss on one interface (30% loss)
sudo tc qdisc add dev eth0 root netem loss 30%

# Run iperf through the tunnel
# Server:
iperf3 -s -B 10.7.0.1

# Client:
iperf3 -c 10.7.0.1 -t 30

# Check FEC stats
curl http://127.0.0.1:8007/api/stats | jq '{fec_recovered, fec_failed, reorder_gaps}'

# Clean up
sudo tc qdisc del dev eth0 root
```

### Test 5: Reorder Buffer

Simulate path latency differential and verify in-order delivery.

```bash
# Add 50ms delay to WiFi path
sudo tc qdisc add dev wlan0 root netem delay 50ms

# Run traffic
ping -c 100 10.7.0.1

# Check reorder stats
curl http://127.0.0.1:8007/api/stats | jq '{reorder_in_order, reorder_reordered, reorder_gaps, reorder_window_ms}'

# Clean up
sudo tc qdisc del dev wlan0 root
```

### Test 6: Audio (SIP Reporter Integration)

Test with actual RTP audio through the tunnel.

```bash
# On the field device, configure SIP Reporter to bind to tunnel IP:
# In settings: bind_address = 10.7.0.2

# On the server, route SIP traffic to Kamailio:
sudo iptables -t nat -A PREROUTING -d 10.7.0.1 -p udp --dport 5060 \
  -j DNAT --to-destination 127.0.0.1:5060

# Make a test call, then pull interfaces during the call
# Audio should continue seamlessly
```

## Management API

The management API runs on `127.0.0.1:8007` by default. Change with `BOND_API` env var.

```bash
# Full stats
curl -s http://127.0.0.1:8007/api/stats | jq .
# {
#   "tx_packets": 15234,
#   "rx_packets": 15200,
#   "fec_recovered": 12,
#   "fec_failed": 0,
#   "reorder_in_order": 14800,
#   "reorder_reordered": 388,
#   "reorder_gaps": 2,
#   "reorder_window_ms": 45,
#   "paths": [
#     { "path_id": 0, "rtt_ms": 12.5, "jitter_ms": 1.2, "loss": 0.001, "rx_count": 15200 }
#   ]
# }

# Per-path health
curl -s http://127.0.0.1:8007/api/paths | jq .

# Config
curl -s http://127.0.0.1:8007/api/config | jq .

# Health check
curl -s http://127.0.0.1:8007/api/health
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BOND_FEC` | `1` (enabled) | Set to `0` to disable FEC |
| `BOND_REORDER` | `1` (enabled) | Set to `0` to disable reorder buffer |
| `BOND_API` | `127.0.0.1:8007` | Management API listen address |
| `LOG_LEVEL` | `error` | Set to `verbose` for debug logging |

## Troubleshooting

### No traffic on bond paths
```bash
# Check bond paths are configured
sudo wg show bond0

# Verify local IPs exist
ip addr show eth0     # should show the @localip you configured
ip addr show wlan0

# Check for firewall rules blocking UDP
sudo iptables -L -n | grep 51820
```

### FEC not recovering
```bash
# Check FEC stats
curl -s http://127.0.0.1:8007/api/stats | jq '{fec_recovered, fec_failed}'

# fec_failed > 0 means loss exceeds FEC capacity
# Consider: loss on ALL paths simultaneously exceeds M parity packets
# Increase FEC ratio or add more paths
```

### High reorder gaps
```bash
# Check reorder window vs path latency
curl -s http://127.0.0.1:8007/api/stats | jq '{reorder_window_ms}'
curl -s http://127.0.0.1:8007/api/paths | jq '.[].rtt_ms'

# If RTT >> window, the window is adapting (increases 10% per gap)
# Should stabilise after ~30 seconds of traffic
```

### Tunnel works but slow
```bash
# Check MTU — 007 reduces it by 13 bytes for FEC overhead
ip link show bond0    # should show mtu 1407 (1420 - 13)

# If MTU is too low, application fragments more
# Ensure outer link MTU is at least 1500
```

## Cleanup

```bash
# Stop 007
sudo kill $(pgrep 007)

# Remove TUN interface
sudo ip link del bond0

# Remove artificial network conditions
sudo tc qdisc del dev eth0 root 2>/dev/null
sudo tc qdisc del dev wlan0 root 2>/dev/null
```

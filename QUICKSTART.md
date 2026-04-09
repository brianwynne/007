# 007 Bond -- Quick Start

## 1. Install Server

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-server.sh | sudo -E bash
```

Dependencies (`wireguard-tools`, `curl`, `jq`, `xxd`) are installed automatically.

## 2. Generate Client Token

```bash
sudo 007-bond enroll-token
```

Copy the client install command from the output. Each token is one-time use and auto-assigns the next available tunnel IP.

## 3. Install Client

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh | \
  sudo ENROLL_URL=http://<server_ip>:8017 ENROLL_TOKEN=<token> bash
```

The client generates its own WireGuard keypair locally (private key never leaves the device), exchanges public keys with the server, and auto-detects all network interfaces for multi-path bonding.

## 4. Verify

```bash
ping 10.7.0.1              # from client -- tunnel connectivity
sudo 007-bond status        # check service, peers, preset
sudo 007-bond stats         # view FEC/ARQ/jitter stats
sudo 007-bond paths         # view per-path health (RTT, loss, jitter)
```

## 5. Set Latency Preset

```bash
sudo 007-bond preset broadcast   # 40ms  -- live broadcast
sudo 007-bond preset studio      # 80ms  -- studio links
sudo 007-bond preset field       # 200ms -- WiFi + cellular (default)
```

Changes apply instantly to both sides via control packet -- no restart needed. Each peer has an independent jitter buffer.

## 6. Route SIP/RTP Through Tunnel (Optional)

To route specific hosts through the bond tunnel for SIP/RTP:

```bash
sudo 007-bond route add sip.rtegroup.ie
```

Or set in `/etc/007/.env`:
```bash
BOND_ROUTES=sip.rtegroup.ie,54.220.131.205
```

`setup-wg.sh` auto-resolves hostnames and adds IPs to WireGuard allowed-ips on restart.

If the 007 server is separate from the SIP server, enable gateway mode on the server:
```bash
sudo 007-bond gateway on    # NATs tunnel traffic to internet
```

## 7. Gateway Mode (Server Only)

When clients need to reach hosts beyond the 007 server itself:

```bash
sudo 007-bond gateway on     # enable NAT (iptables MASQUERADE + FORWARD)
sudo 007-bond gateway off    # disable, restrict to tunnel-only
```

## Useful Commands

```bash
sudo 007-bond status          # service, peers, preset
sudo 007-bond stats           # FEC, ARQ, jitter stats
sudo 007-bond paths           # per-path health
sudo 007-bond route add <host>  # route host through tunnel
sudo 007-bond route del <host>  # remove bond route
sudo 007-bond route flush       # remove all bond routes
sudo 007-bond gateway on|off    # NAT gateway mode (server)
sudo 007-bond logs            # tail logs
sudo 007-bond restart         # restart service
sudo 007-bond upgrade         # upgrade to latest
```

## Prerequisites

- Linux kernel bonding module (`bonding.ko`) must NOT be loaded. If present, blacklist it: `echo "blacklist bonding" > /etc/modprobe.d/007-no-bonding.conf`
- The installer handles all other prerequisites (wireguard-tools, ARP sysctl, policy routing)

## Troubleshooting

If you see issues after upgrading, try a clean install -- some "regressions" (e.g. v0.5.4) turned out to be stale state from previous testing, not code bugs. A clean install resolves them.

Full documentation: [INSTALL.md](docs/INSTALL.md) | [IMPLEMENTATION.md](docs/IMPLEMENTATION.md)

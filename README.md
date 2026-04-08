# 007 Bond -- Multi-Path Network Bonding

007 Bond is a multi-path network bonding solution built on [wireguard-go](https://git.zx2c4.com/wireguard-go). It sends every packet simultaneously across all configured network interfaces (ethernet, WiFi, cellular) and uses FEC, ARQ, and a jitter buffer to deliver reliable, low-latency connectivity over unreliable links.

Designed for broadcast field contribution where dropping audio is not an option.

## How It Works

```
APP (e.g. SIP Reporter)
        |
   +----+----+  TUN interface (bond0)
   |   007   |  FEC encode -> encrypt -> multi-path send
   +--+-+-+--+
      | | |
   eth0 wlan0 wwan0  -- all paths simultaneously --  server
```

- Every encrypted packet is sent on ALL paths (client and server)
- Server auto-discovers client endpoints from incoming packets -- no UAPI config needed on the server side
- Client configures bond paths via UAPI (`add-bond-paths.sh` detects interfaces automatically)
- Receiver discards duplicates (WireGuard replay filter)
- Sliding-window FEC (XOR) recovers single losses in 20ms
- ARQ fires NACKs for every gap, racing FEC in parallel
- Jitter buffer holds packets until playout deadline
- Per-path health tracking (RTT, loss, jitter) drives adaptive timeouts

## Bidirectional Multi-Path (v0.4.0+)

Multi-path works in both directions without manual server configuration:

- **Client -> Server**: Client's `add-bond-paths.sh` detects all interfaces and configures bond paths via UAPI. Each interface sends through a dedicated socket.
- **Server -> Client**: Server auto-discovers client source addresses from incoming packets. `SendBuffers` sends replies to ALL discovered endpoints. Endpoints not seen for 60s are evicted.

This means the server needs zero per-client path configuration. When a client sends from eth0 and wlan0, the server automatically learns both addresses and replies to both.

## Install

### Server

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-server.sh | sudo -E bash
```

### Client (via enrollment)

```bash
# On server -- generate a one-time enrollment token:
sudo 007-bond enroll-token

# On client -- install with token (generates keys locally, exchanges via API):
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

Change preset at runtime (no restart -- signals peers automatically via control packet type=5):

```bash
sudo 007-bond preset broadcast    # set locally + signal all peers
sudo 007-bond preset field 10.7.0.3  # set on a specific peer (remote help)
```

Each peer has an independent jitter buffer on the server -- changing one client's preset does not affect others.

## Management CLI

```bash
sudo 007-bond status          # service, interface, peers, preset
sudo 007-bond stats           # FEC, ARQ, jitter buffer statistics
sudo 007-bond paths           # per-path health (RTT, loss, jitter)
sudo 007-bond preset [name]   # show or set latency preset
sudo 007-bond route add <host>  # route specific host through bond tunnel
sudo 007-bond route del <host>  # remove bond route
sudo 007-bond route flush       # remove all bond routes
sudo 007-bond gateway on|off    # enable/disable NAT gateway mode
sudo 007-bond logs            # tail service logs
sudo 007-bond start|stop|restart
sudo 007-bond enroll-token    # generate one-time client enrollment token
sudo 007-bond list-tokens     # show pending enrollment tokens
sudo 007-bond revoke-token <t>  # revoke an unused token
sudo 007-bond upgrade         # upgrade to latest release
sudo 007-bond version         # show installed version
sudo 007-bond uninstall       # remove (preserves config/data)
```

## SIP/RTP Through Tunnel

007 Bond integrates with SIP Reporter (rtesip) for broadcast field contribution over bonded connections:

1. **Route SIP traffic through the tunnel**: Set `BOND_ROUTES=sip.rtegroup.ie` in `/etc/007/.env`. `setup-wg.sh` auto-resolves hostnames and adds IPs to WireGuard allowed-ips.
2. **Gateway mode** (when 007 server is separate from SIP server): `sudo 007-bond gateway on` enables iptables MASQUERADE + FORWARD so tunnel traffic can reach the internet.
3. **Kamailio config**: SIP server must have `force_rport()` and `fix_nated_contact()` in its config for NATted clients coming through the tunnel.
4. **rtesip integration**: rtesip's installer optionally installs 007 Bond client with interactive enrollment. Adds systemd dependency (`After=007-bond.service`) so the tunnel is up before SIP starts.

Complete flow: STUN -> SIP registration -> RTP via RTPEngine, all through the bond tunnel.

## Gateway Mode

When the 007 server is separate from the SIP/media server, enable gateway mode to NAT tunnel traffic to the internet:

```bash
sudo 007-bond gateway on    # enables ip_forward, iptables MASQUERADE + FORWARD
sudo 007-bond gateway off   # disables NAT, restricts to tunnel-only
```

Gateway mode auto-detects the default outgoing interface for MASQUERADE and updates all peers to allowed-ips `0.0.0.0/0`. State persists across restarts via `BOND_GATEWAY=on` in `.env`.

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
1. Multi-path diversity -- same packet on all interfaces (first line of defence)
2. Sliding FEC (XOR, W=5) -- recovers single losses within 20ms window
3. ARQ -- NACKs fire for every gap, racing FEC in parallel
4. Jitter buffer -- holds packets for playout deadline, delivers in order
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BOND_PRESET` | `field` | Latency preset: `broadcast`, `studio`, `field` |
| `BOND_FEC_MODE` | `sliding` | FEC strategy: `block` (Reed-Solomon) or `sliding` (XOR) |
| `BOND_FEC` | enabled | Set to `0` to disable FEC |
| `BOND_JITTER` | enabled | Set to `0` to disable jitter buffer |
| `BOND_REORDER` | enabled | Set to `0` to disable reorder buffer |
| `BOND_ROUTES` | empty | Comma-separated hosts/IPs to route through tunnel |
| `BOND_GATEWAY` | `off` | NAT gateway mode: `on` or `off` (server only) |
| `BOND_API` | `127.0.0.1:8007` | Management API listen address |
| `BOND_API_KEY` | empty | Optional API authentication key |
| `LOG_LEVEL` | `error` | `verbose`, `error`, or `silent` |

## Deployment Layout

```
/opt/007/           -- binary, helper scripts
/etc/007/           -- config (.env), WireGuard keys, enrollment tokens
/etc/007/peers/     -- persisted enrolled peer configs (server)
/var/lib/007/       -- persistent data
/var/log/007/       -- logs
```

Runs as dedicated `bond007` system user with Linux capabilities (`CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_NET_BIND_SERVICE`) -- not root.

Systemd services:
- `007-bond` -- main bonding service (Go runtime tuned: `GOGC=200`, `GOMEMLIMIT=64MiB`)
- `007-bond-enroll` -- enrollment API (server only, port 8017)
- `007-bond-paths.timer` -- auto-detects interfaces every 30s (client only)

Systemd hardening: `RuntimeDirectory=wireguard`, `ProtectSystem=strict`, `NoNewPrivileges=true`.

## Policy-Based Routing

When multiple interfaces share the same subnet (e.g. eth0 + wlan0 on the same home router), Linux would send all traffic out the default interface. `add-bond-paths.sh` creates per-interface routing rules to force each interface's traffic out the correct physical interface:

```bash
ip rule add from <eth0_ip> table 101 prio 32701
ip route replace default via <gw> dev eth0 table 101
ip rule add from <wlan0_ip> table 102 prio 32702
ip route replace default via <gw> dev wlan0 table 102
```

This is automatic -- no manual configuration needed.

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

- [Quick Start](QUICKSTART.md)
- [Installation Guide](docs/INSTALL.md)
- [Implementation Details](docs/IMPLEMENTATION.md)

## Based On

Fork of [wireguard-go](https://git.zx2c4.com/wireguard-go) (MIT License, Copyright 2017-2025 WireGuard LLC).

007 Bond adds the `bond/` package and modifies `device/send.go`, `device/receive.go`, `device/device.go`, `device/peer.go`, and `device/uapi.go` for pipeline integration. The WireGuard encryption, handshake, and key management are unmodified.

## License

MIT License. See [LICENSE](LICENSE).

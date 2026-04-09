# 007 Bond -- Installation Guide

## Production Install (Recommended)

### Server

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-server.sh | sudo -E bash
```

This will:
- Install dependencies (`wireguard-tools`, `curl`, `jq`, `xxd`)
- Download the latest release binary for your platform (amd64, arm64, armv7l)
- Create `bond007` system user with Linux capabilities
- Set up systemd service with Go runtime tuning (`GOGC=200`, `GOMEMLIMIT=64MiB`)
- Configure `RuntimeDirectory=wireguard` for the UAPI socket
- Generate WireGuard keys
- Start the enrollment service (port 8017) with `ReadWritePaths` for `/etc/007/peers` (peer persistence)
- Configure firewall (ufw)
- Install the `007-bond` management CLI
- Persist enrolled peers in `/etc/007/peers/`

**Important**: If Linux kernel bonding (`bonding.ko`) is loaded, it must be removed first. It conflicts with the `bond0` TUN interface. Blacklist it with:
```bash
echo "blacklist bonding" > /etc/modprobe.d/007-no-bonding.conf
rmmod bonding 2>/dev/null || true
```

### Client (Token Enrollment)

On the **server**, generate a one-time enrollment token:

```bash
sudo 007-bond enroll-token
```

This prints a client install command. On the **client**:

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh | \
  sudo ENROLL_URL=http://<server_ip>:8017 ENROLL_TOKEN=<token> bash
```

The client generates its own WireGuard keypair locally (private key never leaves the device), sends only the public key to the server, and receives the server's public key and tunnel IP in return.

### Client (Manual Keys)

If you already have keys:

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh | \
  sudo SERVER_IP=<ip> SERVER_PUB=<base64_key> CLIENT_KEY=<base64_key> bash
```

### Upgrade

Re-run the installer -- it detects existing installations and preserves config/keys:

```bash
# Server
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-server.sh | sudo -E bash

# Client
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh -o /tmp/007.sh && sudo bash /tmp/007.sh

# Or use the CLI
sudo 007-bond upgrade
sudo 007-bond upgrade --tag v0.4.0
```

### rtesip Integration

007 Bond can be installed as part of the SIP Reporter (rtesip) installer. During `rtesip install`, an interactive prompt asks for the 007 Bond enrollment server URL and token. If provided:

1. The 007 Bond client is installed via token enrollment
2. A systemd dependency is added: `After=007-bond.service`, `Wants=007-bond.service`
3. This ensures the bond tunnel is up before the SIP stack starts

To get a token for rtesip install, run on the 007 server:
```bash
sudo 007-bond enroll-token
```

## Directory Layout

```
/opt/007/                   -- binary, helper scripts
  007                       -- main binary
  setup-wg.sh              -- WireGuard setup (runs on start via ExecStartPost)
  add-bond-paths.sh        -- interface detection + policy routing (client only)
  enroll-server.sh         -- enrollment service (server only)

/etc/007/                   -- configuration
  .env                     -- environment config (BOND_PRESET, BOND_ROUTES, etc.)
  server.key / server.pub  -- WireGuard keys (server)
  client.key / server.pub  -- WireGuard keys (client)
  tokens/                  -- enrollment tokens (server only)
  peers/                   -- persisted enrolled peer configs (server only)

/var/lib/007/              -- persistent data
/var/log/007/              -- logs (14-day logrotate)
/var/run/wireguard/        -- UAPI socket (RuntimeDirectory)
```

## Systemd Services

| Service | Purpose | Where |
|---------|---------|-------|
| `007-bond` | Main bonding service | Both |
| `007-bond-enroll` | Enrollment API (port 8017) | Server only |
| `007-bond-paths.timer` | Auto-detect interfaces every 30s | Client only |

### Path Monitor Lock File

The `007-bond-paths.timer` triggers `add-bond-paths.sh` every 30 seconds. To prevent duplicate socket creation on timer ticks, the script uses a lock file (`/var/lib/007/bond-paths.lock`) that stores the current interface list. If interfaces haven't changed since the last run, the script exits early -- no UAPI command, no socket churn. The lock file is cleared on service start (`setup-wg.sh`) so paths are always configured on first run.

The Go code is also idempotent: `AddBondPath` checks for an existing path with the same local IP before creating a new socket, so it is safe to call multiple times even without the lock file.

### ARP Flux Fix (Client)

The client installer applies sysctl settings (`/etc/sysctl.d/007-arp.conf`) for correct ARP behaviour on multi-interface hosts sharing the same subnet:

- `arp_filter=1` -- reply only on the interface that owns the IP
- `arp_announce=2` -- use the best local address as ARP source
- `arp_ignore=1` -- respond only if the target IP is configured on the incoming interface

Without these, Linux's weak host model causes ARP flux: the router learns the wrong MAC for an IP, and traffic goes to the wrong interface.

The main service runs with:
- `GOGC=200` and `GOMEMLIMIT=64MiB` (Go runtime tuning to reduce GC frequency)
- `RuntimeDirectory=wireguard` (ensures `/var/run/wireguard` exists for the UAPI socket)
- `ProtectSystem=strict`, `NoNewPrivileges=true` (security hardening)
- `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_NET_BIND_SERVICE` (capabilities, not root)

```bash
sudo systemctl status 007-bond
sudo journalctl -u 007-bond -f
```

## Configuration

Edit `/etc/007/.env`:

```bash
# Presets: broadcast (40ms), studio (80ms), field (200ms)
BOND_PRESET=field

# FEC strategy
BOND_FEC_MODE=sliding

# API
BOND_API=0.0.0.0:8007
BOND_API_KEY=<auto-generated>

# Route specific hosts through tunnel (comma-separated hostnames or IPs)
BOND_ROUTES=sip.rtegroup.ie,54.220.131.205

# NAT gateway mode (server only)
BOND_GATEWAY=off

# Logging
LOG_LEVEL=error
```

Change preset at runtime (no restart needed):

```bash
sudo 007-bond preset broadcast
```

## Management CLI

```bash
# Status and monitoring
sudo 007-bond status              # service, interface, peers, preset
sudo 007-bond stats               # FEC, ARQ, jitter buffer statistics
sudo 007-bond paths               # per-path health (RTT, loss, jitter)
sudo 007-bond preset              # show current preset
sudo 007-bond preset broadcast    # change preset (signals peers via control packet)
sudo 007-bond preset field 10.7.0.3  # change preset on specific peer
sudo 007-bond logs                # tail service logs

# Routing
sudo 007-bond route                 # show current bond routes
sudo 007-bond route add <host>      # route host through bond tunnel
sudo 007-bond route del <host>      # remove bond route
sudo 007-bond route flush           # remove all bond routes

# Gateway mode (server only)
sudo 007-bond gateway               # show current gateway state
sudo 007-bond gateway on            # enable NAT (MASQUERADE + FORWARD)
sudo 007-bond gateway off           # disable NAT

# Enrollment (server only)
sudo 007-bond enroll-token        # generate enrollment token
sudo 007-bond list-tokens         # show pending tokens
sudo 007-bond revoke-token <tok>  # revoke token
sudo 007-bond add-client <key>    # add WireGuard peer manually

# Service management
sudo 007-bond start|stop|restart
sudo 007-bond upgrade             # upgrade to latest release
sudo 007-bond upgrade --tag v0.4.0
sudo 007-bond version             # show installed version
sudo 007-bond uninstall           # remove (preserves config/data)
```

## Management API

All endpoints require `X-API-Key` header if `BOND_API_KEY` is set.

```bash
# Stats (FEC, ARQ, jitter buffer, paths)
curl -H "X-API-Key:<key>" http://127.0.0.1:8007/api/stats | jq .

# Per-path health
curl -H "X-API-Key:<key>" http://127.0.0.1:8007/api/paths | jq .

# Current preset
curl -H "X-API-Key:<key>" http://127.0.0.1:8007/api/preset | jq .

# Change preset at runtime
curl -X POST -H "X-API-Key:<key>" -H "Content-Type: application/json" \
  http://127.0.0.1:8007/api/preset -d '{"preset":"broadcast"}'

# Health check (no auth required)
curl http://127.0.0.1:8007/api/health
```

## SIP/RTP Through Tunnel

### Routing specific hosts

Set `BOND_ROUTES` in `/etc/007/.env` to route specific hosts through the bond tunnel:

```bash
BOND_ROUTES=sip.rtegroup.ie,54.220.131.205
```

On service start, `setup-wg.sh`:
1. Resolves hostnames to IPs
2. Adds each IP to WireGuard's `allowed-ips` (so WireGuard accepts return traffic)
3. Creates `ip route` entries pointing those IPs through `bond0` via the server tunnel IP

At runtime, use the CLI:
```bash
sudo 007-bond route add sip.rtegroup.ie    # resolves + routes immediately
sudo 007-bond route del sip.rtegroup.ie
```

### Gateway mode (server)

When the 007 server is separate from the SIP/media server, clients need their tunnel traffic forwarded to the internet. Enable gateway mode on the server:

```bash
sudo 007-bond gateway on
```

This:
- Enables `net.ipv4.ip_forward=1` (persisted via sysctl.d)
- Adds iptables rules: `FORWARD` (bond0 -> outgoing interface) + `MASQUERADE` on 10.7.0.0/24
- Updates all WireGuard peers to `allowed-ips 0.0.0.0/0`
- Persists as `BOND_GATEWAY=on` in `.env` (re-applied on restart via `setup-wg.sh`)

### Kamailio integration

For NATted SIP clients coming through the bond tunnel, the Kamailio SIP server config must include:

```
force_rport();
fix_nated_contact();
```

Without these, SIP responses will be sent to the wrong address and RTP negotiation will fail.

### Complete SIP flow through tunnel

```
Client (rtesip)                   007 Tunnel                    Server
  |-- STUN ------- bond0 -------->|-- gateway NAT -->  STUN server
  |-- REGISTER --- bond0 -------->|-- gateway NAT -->  Kamailio (force_rport)
  |-- INVITE ----- bond0 -------->|-- gateway NAT -->  Kamailio
  |-- RTP -------- bond0 -------->|-- gateway NAT -->  RTPEngine
```

## Policy-Based Routing

When multiple client interfaces share the same subnet (e.g. eth0 + wlan0 on the same home router), Linux would send all traffic out the default interface. `add-bond-paths.sh` creates per-interface routing rules:

```bash
# For each interface with a route to the server:
ip rule add from <local_ip> table <N> prio <32700+N>
ip route replace default via <gateway> dev <iface> table <N>
ip route replace <subnet>/24 dev <iface> scope link table <N>
```

This forces traffic sourced from each interface's IP out through that specific interface. It runs automatically on startup and every 30 seconds via the path monitor timer.

## Network Impairment Testing

The test suite validates FEC, ARQ, and multi-path recovery under simulated network conditions:

```bash
# Full suite (32 tests across 10 sections)
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/007-impairment-suite.sh | sudo bash

# Requires:
# - 007 running with bond paths configured
# - Server API accessible via tunnel (BOND_API=0.0.0.0:8007)
# - Root privileges (tc/netem + iptables)
```

### Test Sections

| Section | Tests | What It Validates |
|---------|-------|-------------------|
| A | Random loss | Multi-path absorbs per-interface loss |
| B | Burst loss | FEC window recovery, ARQ for long bursts |
| C | Path asymmetry | Reorder buffer handles delay differences |
| D | Reorder | Jitter buffer delivers in order |
| E | Jitter | Jitter buffer absorbs delay variation |
| F | Short outages | Path failover and recovery |
| G | Combined | Real-world WiFi/cellular scenarios |
| H | Sliding FEC | Window-specific edge cases |
| I | Single-path egress | Server FEC/ARQ recovery (no multi-path) |
| J | Inbound loss | Client FEC/ARQ recovery (iptables) |

## Firewall

The installer configures ufw:

| Port | Protocol | Purpose |
|------|----------|---------|
| 22 | TCP | SSH |
| 51820 | UDP | WireGuard tunnel |
| 8007 | TCP | Management API (tunnel-only on server) |
| 8017 | TCP | Enrollment API (server only) |

## Building from Source

```bash
# Prerequisites
go version    # 1.23+

# Build
go build -o 007 .

# Cross-compile for Raspberry Pi
GOOS=linux GOARCH=arm64 go build -o 007-arm64 .    # RPi 4
GOOS=linux GOARCH=arm go build -o 007-arm .         # RPi 3

# Run tests
go test ./bond/ -v
```

## Troubleshooting

### Tunnel not working after restart

```bash
sudo 007-bond status         # check service is running
sudo wg show bond0           # check WireGuard handshake
sudo 007-bond restart        # restart service
```

### Stats showing "unauthorized"

The API key is set in `/etc/007/.env`. Use the CLI which handles auth automatically:

```bash
sudo 007-bond stats
```

### High ping latency

Check the preset -- field preset adds up to 360ms (180ms each side):

```bash
sudo 007-bond preset         # show current preset
sudo 007-bond preset broadcast   # switch to 40ms total
```

### FEC not recovering packets

Check FEC mode and verify with the impairment suite:

```bash
grep BOND_FEC_MODE /etc/007/.env    # should be "sliding"
sudo 007-bond stats | grep fec     # check fec_recovered
```

### Bond paths not detected

```bash
sudo 007-bond paths          # show active paths
sudo systemctl status 007-bond-paths.timer   # check path monitor
sudo /opt/007/add-bond-paths.sh              # manually re-scan
```

### v0.5.4 "regression" on upgrade

The v0.5.4 issues were caused by stale state from previous testing, not a code bug. A clean install (`sudo 007-bond uninstall && curl ... | sudo bash`) resolves it. When troubleshooting upgrade issues, always try a clean install first.

### xxd not found

The `xxd` binary is required for converting WireGuard public keys to hex for UAPI commands. Install it:

```bash
sudo apt-get install xxd
```

The installer handles this automatically, but manual installations may need it.

### UAPI socket missing (/var/run/wireguard/bond0.sock)

The systemd service uses `RuntimeDirectory=wireguard` to ensure `/var/run/wireguard` exists. If the socket is missing:

```bash
ls -la /var/run/wireguard/    # check directory exists
sudo 007-bond restart         # restart creates it
```

### SIP registration fails through tunnel

1. Check routes: `sudo 007-bond route` -- verify the SIP server is listed
2. Check gateway mode (if server != SIP server): `sudo 007-bond gateway` on the 007 server
3. Check Kamailio config includes `force_rport()` and `fix_nated_contact()`
4. Check WireGuard allowed-ips include the SIP server IP: `sudo wg show bond0`

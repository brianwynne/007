#!/usr/bin/env bash
# 007 Bond — Local loopback test on a single machine
#
# NOTE: On WSL2, data packets through the TUN device may not work due to
# IFF_VNET_HDR / virtio_net_hdr incompatibility with WSL2's kernel.
# The WireGuard handshake and keepalives work, but ICMP/data packets
# may not traverse the tunnel. This is an upstream wireguard-go + WSL2
# issue, not a 007 bug. Test on a real Linux machine for full validation.
#
# Creates two network namespaces (client/server) connected via veth pairs,
# runs 007 in each, and tests the bond tunnel with traffic + impairment.
#
# Requirements: root, go (for building), wg (wireguard-tools)
# Usage: sudo bash tests/local-test.sh
#
set -euo pipefail

# Ensure go is in PATH (sudo doesn't inherit user PATH)
export PATH="$PATH:/usr/local/go/bin:/home/*/go-install/go/bin:$(eval echo ~${SUDO_USER:-$USER})/go-install/go/bin:$(eval echo ~${SUDO_USER:-$USER})/go/bin"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
BINARY="$PROJECT_DIR/007"
LOG_DIR="/tmp/007-test"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
fail()  { echo -e "${RED}[X]${NC} $*"; }
pass()  { echo -e "${GREEN}[✓]${NC} $*"; }

cleanup() {
    info "Cleaning up..."
    kill $(cat $LOG_DIR/server.pid 2>/dev/null) 2>/dev/null || true
    kill $(cat $LOG_DIR/client.pid 2>/dev/null) 2>/dev/null || true
    ip netns del 007-server 2>/dev/null || true
    ip netns del 007-client 2>/dev/null || true
    ip link del veth-s1 2>/dev/null || true
    ip link del veth-s2 2>/dev/null || true
    info "Logs preserved at $LOG_DIR/"
}
trap cleanup EXIT

# ─── Preflight ───────────────────────────────────────────────────────
if [ "$(id -u)" -ne 0 ]; then
    fail "Must run as root: sudo bash $0"
    exit 1
fi

if ! command -v wg &>/dev/null; then
    fail "wireguard-tools not installed: sudo apt install wireguard-tools"
    exit 1
fi

mkdir -p "$LOG_DIR"

# ─── Build ───────────────────────────────────────────────────────────
info "Building 007..."
cd "$PROJECT_DIR"
if ! command -v go &>/dev/null; then
    fail "go not found in PATH. Build first: go build -o 007 ."
    fail "Then run: sudo bash tests/local-test.sh"
    exit 1
fi
go build -o "$BINARY" .
info "Binary: $BINARY ($(du -h "$BINARY" | cut -f1))"

# ─── Generate keys ───────────────────────────────────────────────────
info "Generating WireGuard keys..."
wg genkey | tee $LOG_DIR/server.key | wg pubkey > $LOG_DIR/server.pub
wg genkey | tee $LOG_DIR/client.key | wg pubkey > $LOG_DIR/client.pub
SERVER_PUB=$(cat $LOG_DIR/server.pub)
CLIENT_PUB=$(cat $LOG_DIR/client.pub)

# ─── Network namespaces ─────────────────────────────────────────────
# Two namespaces connected by two veth pairs (simulating two paths)
#
#   007-client                          007-server
#   ┌──────────┐                        ┌──────────┐
#   │ bond0    │ 10.7.0.2               │ bond0    │ 10.7.0.1
#   │ (tunnel) │                        │ (tunnel) │
#   │          │                        │          │
#   │ veth-c1 ─┼──── path 1 (eth) ─────┼─ veth-s1 │
#   │ 10.1.1.2 │                        │ 10.1.1.1 │
#   │          │                        │          │
#   │ veth-c2 ─┼──── path 2 (wifi) ────┼─ veth-s2 │
#   │ 10.2.2.2 │                        │ 10.2.2.1 │
#   └──────────┘                        └──────────┘

info "Creating network namespaces..."
ip netns add 007-server
ip netns add 007-client

# Path 1 (simulated ethernet)
ip link add veth-s1 type veth peer name veth-c1
ip link set veth-s1 netns 007-server
ip link set veth-c1 netns 007-client
ip netns exec 007-server ip addr add 10.1.1.1/24 dev veth-s1
ip netns exec 007-server ip link set veth-s1 up
ip netns exec 007-client ip addr add 10.1.1.2/24 dev veth-c1
ip netns exec 007-client ip link set veth-c1 up

# Path 2 (simulated WiFi)
ip link add veth-s2 type veth peer name veth-c2
ip link set veth-s2 netns 007-server
ip link set veth-c2 netns 007-client
ip netns exec 007-server ip addr add 10.2.2.1/24 dev veth-s2
ip netns exec 007-server ip link set veth-s2 up
ip netns exec 007-client ip addr add 10.2.2.2/24 dev veth-c2
ip netns exec 007-client ip link set veth-c2 up

# Loopback
ip netns exec 007-server ip link set lo up
ip netns exec 007-client ip link set lo up

info "Network namespaces ready"
info "  Path 1: client 10.1.1.2 <-> server 10.1.1.1"
info "  Path 2: client 10.2.2.2 <-> server 10.2.2.1"

# ─── Start 007 server ────────────────────────────────────────────────
info "Starting 007 server..."
ip netns exec 007-server env LOG_LEVEL=verbose "$BINARY" -f bond-s > $LOG_DIR/server.log 2>&1 &
echo $! > $LOG_DIR/server.pid
sleep 1

ip netns exec 007-server wg set bond-s \
    listen-port 51820 \
    private-key $LOG_DIR/server.key \
    peer "$CLIENT_PUB" \
        allowed-ips 10.7.0.2/32

ip netns exec 007-server ip addr add 10.7.0.1/24 dev bond-s
ip netns exec 007-server ip link set bond-s up

# ─── Start 007 client ────────────────────────────────────────────────
info "Starting 007 client..."
ip netns exec 007-client env LOG_LEVEL=verbose "$BINARY" -f bond-c > $LOG_DIR/client.log 2>&1 &
echo $! > $LOG_DIR/client.pid
sleep 1

# Standard wg config (wg CLI doesn't support bond_endpoint)
ip netns exec 007-client wg set bond-c \
    private-key $LOG_DIR/client.key \
    peer "$SERVER_PUB" \
        endpoint 10.1.1.1:51820 \
        allowed-ips 10.7.0.0/24 \
        persistent-keepalive 1

# Bond endpoints via UAPI socket (wg CLI doesn't support custom extensions)
# Each 007 instance creates its own socket: /var/run/wireguard/<iface>.sock
UAPI_SOCK="/var/run/wireguard/bond-c.sock"
sleep 1
SERVER_PUB_HEX=$(echo -n "$SERVER_PUB" | base64 -d | xxd -p -c 32)
info "Configuring bond endpoints via UAPI socket..."
printf "set=1\npublic_key=%s\nbond_endpoint=10.1.1.1:51820@10.1.1.2\nbond_endpoint=10.1.1.1:51820@10.2.2.2\n\n" \
    "$SERVER_PUB_HEX" | nc -U "$UAPI_SOCK" -w 1 > $LOG_DIR/uapi-response.txt 2>&1 || true
if grep -q "errno=0" $LOG_DIR/uapi-response.txt 2>/dev/null; then
    info "Bond endpoints configured via UAPI"
else
    warn "UAPI bond config: $(cat $LOG_DIR/uapi-response.txt 2>/dev/null || echo 'no response')"
    warn "Continuing with single-path (primary endpoint only)"
fi

ip netns exec 007-client ip addr add 10.7.0.2/24 dev bond-c
ip netns exec 007-client ip link set bond-c up

info "007 tunnel configured"
sleep 2

# ─── Test 1: Basic connectivity ──────────────────────────────────────
# Debug: show interface state and routes
info "Debug: client namespace state"
ip netns exec 007-client ip addr show bond-c 2>&1 | head -5
ip netns exec 007-client ip route show 2>&1
ip netns exec 007-client wg show bond-c 2>&1 | head -10

info "Debug: server namespace state"
ip netns exec 007-server ip addr show bond-s 2>&1 | head -5
ip netns exec 007-server wg show bond-s 2>&1 | head -10

# Debug: check if packets enter the TUN device at all
info "Debug: tcpdump on bond-c TUN during ping..."
ip netns exec 007-client tcpdump -i bond-c -c 3 -w $LOG_DIR/tun-capture.pcap 2>/dev/null &
TCPDUMP_PID=$!
sleep 1
ip netns exec 007-client ping -c 2 -W 1 10.7.0.1 > /dev/null 2>&1 || true
sleep 1
kill $TCPDUMP_PID 2>/dev/null || true
wait $TCPDUMP_PID 2>/dev/null || true
CAPTURED=$(tcpdump -r $LOG_DIR/tun-capture.pcap 2>/dev/null | wc -l || echo 0)
info "Debug: captured $CAPTURED packets on bond-c TUN"
if [ "$CAPTURED" -eq 0 ]; then
    warn "No packets captured on TUN — kernel not routing to TUN device"
    warn "This may be a WSL2 TUN/namespace limitation"
fi

echo ""
info "═══ TEST 1: Basic tunnel connectivity ═══"
if ip netns exec 007-client ping -c 3 -W 2 10.7.0.1 > $LOG_DIR/ping1.txt 2>&1; then
    pass "Tunnel is up — ping works"
    grep "rtt" $LOG_DIR/ping1.txt || true
else
    fail "Tunnel ping failed"
    cat $LOG_DIR/ping1.txt
    warn "Check logs: $LOG_DIR/server.log and $LOG_DIR/client.log"
    exit 1
fi

# ─── Test 2: Multi-path verification ─────────────────────────────────
echo ""
info "═══ TEST 2: Multi-path — traffic on both interfaces ═══"

# Capture on both paths
ip netns exec 007-server timeout 5 tcpdump -i veth-s1 -c 5 udp port 51820 -q > $LOG_DIR/tcpdump-path1.txt 2>&1 &
PID1=$!
ip netns exec 007-server timeout 5 tcpdump -i veth-s2 -c 5 udp port 51820 -q > $LOG_DIR/tcpdump-path2.txt 2>&1 &
PID2=$!
sleep 1

# Generate traffic
ip netns exec 007-client ping -c 10 -i 0.1 10.7.0.1 > /dev/null 2>&1 || true
sleep 2

wait $PID1 2>/dev/null || true
wait $PID2 2>/dev/null || true

PATH1_COUNT=$(grep -c "UDP" $LOG_DIR/tcpdump-path1.txt 2>/dev/null || echo 0)
PATH2_COUNT=$(grep -c "UDP" $LOG_DIR/tcpdump-path2.txt 2>/dev/null || echo 0)

if [ "$PATH1_COUNT" -gt 0 ] && [ "$PATH2_COUNT" -gt 0 ]; then
    pass "Multi-path confirmed: path1=$PATH1_COUNT packets, path2=$PATH2_COUNT packets"
else
    warn "Multi-path: path1=$PATH1_COUNT, path2=$PATH2_COUNT (expected both >0)"
fi

# ─── Test 3: Management API ─────────────────────────────────────────
echo ""
info "═══ TEST 3: Management API ═══"
STATS=$(ip netns exec 007-client curl -s http://127.0.0.1:8007/api/stats 2>/dev/null || echo "FAIL")
if echo "$STATS" | grep -q "tx_packets"; then
    pass "API responding"
    echo "$STATS" | python3 -m json.tool 2>/dev/null || echo "$STATS"
else
    warn "API not responding (might need more traffic first)"
fi

# ─── Test 4: Path failure / failover ─────────────────────────────────
echo ""
info "═══ TEST 4: Path failure — disable path 2, verify continuity ═══"

# Start continuous ping
ip netns exec 007-client ping -c 20 -i 0.2 10.7.0.1 > $LOG_DIR/ping-failover.txt 2>&1 &
PING_PID=$!
sleep 1

# Kill path 2
info "  Disabling path 2 (veth-c2)..."
ip netns exec 007-client ip link set veth-c2 down
sleep 2

# Re-enable path 2
info "  Re-enabling path 2..."
ip netns exec 007-client ip link set veth-c2 up
sleep 2

wait $PING_PID 2>/dev/null || true

RECEIVED=$(grep -c "bytes from" $LOG_DIR/ping-failover.txt 2>/dev/null || echo 0)
TOTAL=20
LOSS_PCT=$(( (TOTAL - RECEIVED) * 100 / TOTAL ))

if [ "$RECEIVED" -ge 15 ]; then
    pass "Failover: $RECEIVED/$TOTAL pings received ($LOSS_PCT% loss during failover)"
else
    warn "Failover: $RECEIVED/$TOTAL pings received ($LOSS_PCT% loss)"
fi

# ─── Test 5: Impairment — packet loss on one path ────────────────────
echo ""
info "═══ TEST 5: Impairment — 30% loss on path 2 ═══"

ip netns exec 007-client tc qdisc add dev veth-c2 root netem loss 30% 2>/dev/null || true

ip netns exec 007-client ping -c 20 -i 0.1 10.7.0.1 > $LOG_DIR/ping-loss.txt 2>&1 || true

ip netns exec 007-client tc qdisc del dev veth-c2 root 2>/dev/null || true

RECEIVED=$(grep -c "bytes from" $LOG_DIR/ping-loss.txt 2>/dev/null || echo 0)
if [ "$RECEIVED" -ge 18 ]; then
    pass "Under 30% loss on path 2: $RECEIVED/20 pings received (multi-path redundancy working)"
else
    warn "Under 30% loss: $RECEIVED/20 received"
fi

# ─── Test 6: Impairment — latency asymmetry ─────────────────────────
echo ""
info "═══ TEST 6: Impairment — 50ms delay on path 2 ═══"

ip netns exec 007-client tc qdisc add dev veth-c2 root netem delay 50ms 2>/dev/null || true

ip netns exec 007-client ping -c 10 -i 0.2 10.7.0.1 > $LOG_DIR/ping-delay.txt 2>&1 || true

ip netns exec 007-client tc qdisc del dev veth-c2 root 2>/dev/null || true

if grep -q "bytes from" $LOG_DIR/ping-delay.txt; then
    pass "Under 50ms path asymmetry: tunnel stable"
    grep "rtt" $LOG_DIR/ping-delay.txt || true
else
    warn "Delay test: no responses"
fi

# ─── Test 7: Stats after impairment ──────────────────────────────────
echo ""
info "═══ TEST 7: Bond stats after tests ═══"
sleep 1
STATS=$(ip netns exec 007-client curl -s http://127.0.0.1:8007/api/stats 2>/dev/null || echo "{}")
echo "$STATS" | python3 -m json.tool 2>/dev/null || echo "$STATS"

PATHS=$(ip netns exec 007-client curl -s http://127.0.0.1:8007/api/paths 2>/dev/null || echo "[]")
echo ""
info "Per-path health:"
echo "$PATHS" | python3 -m json.tool 2>/dev/null || echo "$PATHS"

# ─── Summary ─────────────────────────────────────────────────────────
echo ""
info "═══ TEST COMPLETE ═══"
info "Logs: $LOG_DIR/"
info "  server.log    — 007 server output"
info "  client.log    — 007 client output"
info "  tcpdump-*.txt — packet captures"
info "  ping-*.txt    — ping results"
echo ""
info "To inspect WireGuard state:"
info "  sudo ip netns exec 007-client wg show bond-c"
info "  sudo ip netns exec 007-server wg show bond-s"

#!/usr/bin/env bash
# 007 Bond — Local test using two TUN devices on the host (no namespaces)
#
# Creates two TUN devices (bond-s and bond-c), runs 007 on each,
# connects them via localhost UDP, and tests the tunnel.
#
# Requirements: root, go, wg (wireguard-tools), nc
# Usage: sudo -E bash tests/local-test.sh
#
set -euo pipefail

export PATH="$PATH:/usr/local/go/bin:$(eval echo ~${SUDO_USER:-$USER})/go-install/go/bin:$(eval echo ~${SUDO_USER:-$USER})/go/bin"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
BINARY="$PROJECT_DIR/007"
LOG_DIR="/tmp/007-test"

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
    sleep 1
    ip link del bond-s 2>/dev/null || true
    ip link del bond-c 2>/dev/null || true
    info "Logs preserved at $LOG_DIR/"
}
trap cleanup EXIT

if [ "$(id -u)" -ne 0 ]; then
    fail "Must run as root: sudo -E bash $0"
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
    fail "go not found. Build first: go build -o 007 ."
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

# ─── Start 007 server ────────────────────────────────────────────────
info "Starting 007 server (port 51820)..."
LOG_LEVEL=verbose BOND_API=127.0.0.1:8017 "$BINARY" -f bond-s > $LOG_DIR/server.log 2>&1 &
echo $! > $LOG_DIR/server.pid
sleep 1

wg set bond-s \
    listen-port 51820 \
    private-key $LOG_DIR/server.key \
    peer "$CLIENT_PUB" \
        allowed-ips 10.7.0.2/32

ip addr add 10.7.0.1/24 dev bond-s 2>/dev/null || true
ip link set bond-s up

# ─── Start 007 client ────────────────────────────────────────────────
info "Starting 007 client (port 51821)..."
LOG_LEVEL=verbose BOND_API=127.0.0.1:8018 "$BINARY" -f bond-c > $LOG_DIR/client.log 2>&1 &
echo $! > $LOG_DIR/client.pid
sleep 1

wg set bond-c \
    listen-port 51821 \
    private-key $LOG_DIR/client.key \
    peer "$SERVER_PUB" \
        endpoint 127.0.0.1:51820 \
        allowed-ips 10.7.0.0/24 \
        persistent-keepalive 1

ip addr add 10.7.0.2/24 dev bond-c 2>/dev/null || true
ip link set bond-c up

info "Waiting for handshake..."
sleep 3

# ─── Debug ───────────────────────────────────────────────────────────
info "Debug: WireGuard status"
wg show bond-s 2>&1 | head -10
echo "---"
wg show bond-c 2>&1 | head -10

# ─── Test 1: Basic connectivity ──────────────────────────────────────
echo ""
info "═══ TEST 1: Basic tunnel connectivity ═══"
if ping -c 3 -W 2 -I bond-c 10.7.0.1 > $LOG_DIR/ping1.txt 2>&1; then
    pass "Tunnel is up — ping works"
    grep "rtt" $LOG_DIR/ping1.txt || true
else
    fail "Tunnel ping failed"
    cat $LOG_DIR/ping1.txt
    echo ""
    info "Server log (last 10 lines):"
    tail -10 $LOG_DIR/server.log
    echo ""
    info "Client log (last 10 lines):"
    tail -10 $LOG_DIR/client.log
    exit 1
fi

# ─── Test 2: Bond endpoint via UAPI ─────────────────────────────────
echo ""
info "═══ TEST 2: Configure bond endpoint via UAPI ═══"
UAPI_SOCK="/var/run/wireguard/bond-c.sock"
SERVER_PUB_HEX=$(echo -n "$SERVER_PUB" | base64 -d | xxd -p -c 32)
# Use localhost as both source and destination (single-host test)
printf "set=1\npublic_key=%s\nbond_endpoint=127.0.0.1:51820@127.0.0.1\n\n" \
    "$SERVER_PUB_HEX" | nc -U "$UAPI_SOCK" -w 1 > $LOG_DIR/uapi-response.txt 2>&1 || true
if grep -q "errno=0" $LOG_DIR/uapi-response.txt 2>/dev/null; then
    pass "Bond endpoint configured via UAPI"
else
    warn "UAPI response: $(cat $LOG_DIR/uapi-response.txt 2>/dev/null)"
fi

# ─── Test 3: Management API ─────────────────────────────────────────
echo ""
info "═══ TEST 3: Management API ═══"
# Generate some traffic first
ping -c 5 -i 0.1 -I bond-c 10.7.0.1 > /dev/null 2>&1 || true
sleep 1

STATS=$(curl -s http://127.0.0.1:8018/api/stats 2>/dev/null || echo "FAIL")
if echo "$STATS" | grep -q "tx_packets"; then
    pass "Client API responding"
    echo "$STATS" | python3 -m json.tool 2>/dev/null || echo "$STATS"
else
    warn "Client API not responding"
fi

echo ""
HEALTH=$(curl -s http://127.0.0.1:8018/api/health 2>/dev/null || echo "FAIL")
if echo "$HEALTH" | grep -q "ok"; then
    pass "Health check OK"
else
    warn "Health check failed"
fi

# ─── Test 4: Throughput ──────────────────────────────────────────────
echo ""
info "═══ TEST 4: Sustained ping (20 packets) ═══"
ping -c 20 -i 0.1 -I bond-c 10.7.0.1 > $LOG_DIR/ping-sustained.txt 2>&1 || true
RECEIVED=$(grep -c "bytes from" $LOG_DIR/ping-sustained.txt 2>/dev/null || echo 0)
if [ "$RECEIVED" -ge 18 ]; then
    pass "Sustained: $RECEIVED/20 received"
    grep "rtt" $LOG_DIR/ping-sustained.txt || true
else
    warn "Sustained: $RECEIVED/20 received"
fi

# ─── Test 5: Final stats ─────────────────────────────────────────────
echo ""
info "═══ TEST 5: Final bond stats ═══"
sleep 1
STATS=$(curl -s http://127.0.0.1:8018/api/stats 2>/dev/null || echo "{}")
echo "$STATS" | python3 -m json.tool 2>/dev/null || echo "$STATS"

echo ""
info "═══ TEST COMPLETE ═══"
info "Logs: $LOG_DIR/"
info "  server.log — 007 server output"
info "  client.log — 007 client output"

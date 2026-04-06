#!/usr/bin/env bash
# 007 Bond Proxy — Client Setup + Test
# Uses kernel WireGuard for crypto + 007 proxy for FEC/multi-path
#
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/proxy-client.sh | sudo bash -s -- <server_ip> <server_pub> <client_key>
set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: sudo bash $0 <server_ip> <server_pub_key> <client_private_key>"
    exit 1
fi

SERVER_IP="$1"
SERVER_PUB="$2"
CLIENT_KEY="$3"

echo "[+] Cleaning up..."
killall 007-proxy 2>/dev/null || true
sleep 1
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done

echo "[+] Installing..."
apt-get update -qq && apt-get install -y -qq wireguard-tools > /dev/null 2>&1

mkdir -p /opt/007
cd /opt/007

echo "$CLIENT_KEY" > client.key
echo "$SERVER_PUB" > server.pub

# Build proxy from source
if ! command -v go &>/dev/null; then
    echo "[+] Installing Go..."
    apt-get install -y -qq golang-go > /dev/null 2>&1
fi
if [ ! -d /opt/007/src ]; then
    git clone https://github.com/brianwynne/007.git /opt/007/src 2>/dev/null || (cd /opt/007/src && git pull)
fi
cd /opt/007/src
go build -o /opt/007/007-proxy ./cmd/007-proxy/
cd /opt/007

echo "[+] Setting up kernel WireGuard..."
ip link add wg0 type wireguard
wg set wg0 private-key ./client.key \
    peer "$SERVER_PUB" \
    endpoint 127.0.0.1:51821 \
    allowed-ips 10.7.0.0/24 \
    persistent-keepalive 25
ip addr add 10.7.0.2/24 dev wg0
ip link set wg0 up

echo "[+] Detecting network interfaces..."
PATHS=""
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [ "$iface" = "wg0" ] && continue
    LOCAL_IP=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
    if [ -n "$LOCAL_IP" ]; then
        PATHS="$PATHS --path $iface=$LOCAL_IP"
        echo "  Path: $iface ($LOCAL_IP)"
    fi
done

echo "[+] Starting 007 proxy..."
WG_PORT=$(wg show wg0 listen-port)
./007-proxy \
    --wg-listen "127.0.0.1:51821" \
    --wg-forward "127.0.0.1:$WG_PORT" \
    --listen-port 51822 \
    --remote "$SERVER_IP:51822" \
    $PATHS \
    --api 127.0.0.1:8007 \
    > /tmp/007-proxy.log 2>&1 &
sleep 2

echo "[+] Waiting for handshake..."
sleep 3

echo ""
echo "=== WireGuard Status ==="
wg show wg0
echo ""

echo "=== TEST 1: Ping through tunnel ==="
if ping -c 5 -W 2 10.7.0.1; then
    echo ""
    echo "[OK] Tunnel works with 007 proxy!"
else
    echo ""
    echo "[FAIL] Ping failed"
    echo ""
    echo "--- Proxy log ---"
    tail -20 /tmp/007-proxy.log
    echo ""
    echo "--- WireGuard ---"
    wg show wg0
    exit 1
fi

echo ""
echo "=== TEST 2: Sustained ping ==="
ping -c 20 -i 0.1 10.7.0.1 || true

echo ""
echo "=== TEST 3: Bond stats ==="
curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "(stats unavailable)"

echo ""
echo "=== TEST 4: Path health ==="
curl -s --max-time 3 http://127.0.0.1:8007/api/paths 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "(paths unavailable)"

if [ $(echo "$PATHS" | wc -w) -gt 4 ]; then
    echo ""
    echo "=== TEST 5: Multi-path traffic ==="
    for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
        [ "$iface" = "wg0" ] && continue
        echo -n "  $iface: "
        timeout 3 tcpdump -i "$iface" udp -c 3 -q 2>&1 | grep -c "UDP" || echo "0"
        echo " packets"
    done &
    ping -c 5 -i 0.2 10.7.0.1 > /dev/null 2>&1 || true
    wait 2>/dev/null
fi

echo ""
echo "=== DONE ==="
echo "Proxy log: /tmp/007-proxy.log"
echo "Stats: curl -s http://127.0.0.1:8007/api/stats | python3 -m json.tool"

#!/usr/bin/env bash
# 007 Bond — Client Setup + Test
# Usage: sudo bash 007-client.sh <server_ip> <server_pub> <client_key>
set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: sudo bash $0 <server_ip> <server_pub_key> <client_private_key>"
    exit 1
fi

SERVER_IP="$1"
SERVER_PUB="$2"
CLIENT_KEY="$3"

echo "[+] Cleaning up..."
for pid in $(pgrep -x 007 2>/dev/null) $(pgrep -x 007-proxy 2>/dev/null); do
    kill -9 "$pid" 2>/dev/null || true
done
sleep 1
rm -f /var/run/wireguard/*.sock
ip link del bond0 2>/dev/null || true
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done

echo "[+] Checking dependencies..."
for pkg in wireguard-tools golang-go git iperf3; do
    dpkg -s "$pkg" > /dev/null 2>&1 || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg" 2>/dev/null || true
done

echo "[+] Building 007 from source..."
cd /tmp
rm -rf /opt/007
mkdir -p /opt/007
git clone --depth 1 https://github.com/brianwynne/007.git /opt/007/src
cd /opt/007/src
go build -o /opt/007/007 .
cd /opt/007

echo "$CLIENT_KEY" > client.key
echo "$SERVER_PUB" > server.pub

echo "[+] Starting 007..."
nohup env ${BOND_FEC_MODE:+BOND_FEC_MODE=$BOND_FEC_MODE} \
         ${BOND_JITTER:+BOND_JITTER=$BOND_JITTER} \
         ${BOND_FEC:+BOND_FEC=$BOND_FEC} \
         ${BOND_REORDER:+BOND_REORDER=$BOND_REORDER} \
         /opt/007/007 -f bond0 > /tmp/007.log 2>&1 &
echo $! > /tmp/007.pid
sleep 3

if ! ip link show bond0 > /dev/null 2>&1; then
    echo "[X] bond0 not created. Log:"
    cat /tmp/007.log
    exit 1
fi

echo "[+] Configuring WireGuard..."
wg set bond0 private-key ./client.key \
    peer "$SERVER_PUB" endpoint "$SERVER_IP:51820" \
    allowed-ips 10.7.0.0/24 persistent-keepalive 25
ip addr add 10.7.0.2/24 dev bond0 2>/dev/null || true
ip link set bond0 up

echo "[+] Waiting for handshake..."
sleep 5

echo ""
echo "=== WireGuard Status ==="
wg show bond0
echo ""

echo "=== TEST 1: Ping ==="
if ping -c 5 -W 2 10.7.0.1; then
    echo ""
    echo "[OK] TUNNEL WORKS!"
else
    echo ""
    echo "[FAIL] Ping failed"
    echo ""
    echo "--- Log ---"
    tail -20 /tmp/007.log
    echo ""
    echo "--- WireGuard ---"
    wg show bond0
    echo ""
    echo "--- tcpdump on bond0 ---"
    timeout 5 tcpdump -i bond0 -c 3 -n &
    sleep 1
    ping -c 2 -W 1 10.7.0.1 || true
    wait 2>/dev/null
    exit 1
fi

echo ""
echo "=== TEST 2: Sustained ping ==="
ping -c 20 -i 0.1 10.7.0.1 || true

echo ""
echo "=== TEST 3: Bond stats ==="
curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "(stats unavailable)"

echo ""
echo "=== DONE ==="
echo "Log: tail -f /tmp/007.log"
echo "Stats: curl -s --max-time 3 http://127.0.0.1:8007/api/stats | python3 -m json.tool"

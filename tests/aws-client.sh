#!/usr/bin/env bash
# 007 Bond — AWS Client Setup + Test
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/aws-client.sh | sudo bash -s -- <server_ip> <server_pub> <client_key>
set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: sudo bash $0 <server_ip> <server_pub_key> <client_private_key>"
    exit 1
fi

SERVER_IP="$1"
SERVER_PUB="$2"
CLIENT_KEY="$3"
BOND_DIR="/opt/007"
VERSION="v0.1.0"

echo "[+] Cleaning up any running instances..."
killall 007 007-bond 2>/dev/null || true
sleep 1
rm -f /var/run/wireguard/*.sock
ip link del bond0 2>/dev/null || true
rm -f "$BOND_DIR/007"

echo "[+] Installing 007 Bond client..."
mkdir -p "$BOND_DIR"
apt-get update -qq && apt-get install -y -qq wireguard-tools xxd netcat-openbsd > /dev/null 2>&1

ARCH=$(dpkg --print-architecture)
case "$ARCH" in
    amd64) SUFFIX="linux-amd64" ;;
    arm64) SUFFIX="linux-arm64" ;;
    *) echo "[X] Unsupported: $ARCH"; exit 1 ;;
esac
curl -fsSL "https://github.com/brianwynne/007/releases/download/$VERSION/007-$SUFFIX" -o "$BOND_DIR/007"
chmod +x "$BOND_DIR/007"

echo "[+] Cleaning up old instances..."
killall 007 2>/dev/null || true
sleep 1
rm -f /var/run/wireguard/bond0.sock
ip link del bond0 2>/dev/null || true

echo "[+] Setting up keys..."
cd "$BOND_DIR"
echo "$CLIENT_KEY" > client.key
echo "$SERVER_PUB" > server.pub

echo "[+] Starting 007..."
WG_NO_VNET_HDR=1 WG_TUN_BLOCKING=1 "$BOND_DIR/007" -f bond0 > /tmp/007-client.log 2>&1 &
sleep 2

echo "[+] Configuring WireGuard..."
wg set bond0 private-key ./client.key \
    peer "$SERVER_PUB" endpoint "$SERVER_IP:51820" \
    allowed-ips 10.7.0.0/24 persistent-keepalive 25
ip addr add 10.7.0.2/24 dev bond0 2>/dev/null || true
ip link set bond0 up

echo "[+] Waiting for handshake..."
sleep 3

echo ""
echo "=== WireGuard Status ==="
wg show bond0
echo ""

echo "=== TEST 1: Ping (no bond, raw tunnel) ==="
if ping -c 5 -W 2 10.7.0.1; then
    echo "[OK] Tunnel data works!"
else
    echo "[FAIL] Ping failed — checking logs..."
    echo ""
    echo "--- Client log ---"
    tail -20 /tmp/007-client.log
    echo ""
    echo "--- Transfer stats ---"
    wg show bond0 | grep transfer
    echo ""
    echo "Trying tcpdump on bond0..."
    timeout 5 tcpdump -i bond0 -c 3 &
    sleep 1
    ping -c 2 -W 1 10.7.0.1 || true
    wait 2>/dev/null
    exit 1
fi

echo ""
echo "=== TEST 2: Configure bond endpoints ==="
SERVER_PUB_HEX=$(echo -n "$SERVER_PUB" | base64 -d | xxd -p -c 32)
BOND_CMD="set=1\npublic_key=$SERVER_PUB_HEX\n"
PATH_COUNT=0
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [ "$iface" = "bond0" ] && continue
    LOCAL_IP=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
    if [ -n "$LOCAL_IP" ]; then
        BOND_CMD="${BOND_CMD}bond_endpoint=${SERVER_IP}:51820@${LOCAL_IP}\n"
        echo "[+] Bond path: $iface ($LOCAL_IP)"
        PATH_COUNT=$((PATH_COUNT + 1))
    fi
done

if [ "$PATH_COUNT" -gt 0 ]; then
    printf "${BOND_CMD}\n" | nc -U /var/run/wireguard/bond0.sock -w 1 > /tmp/007-uapi.txt 2>&1 || true
    if grep -q "errno=0" /tmp/007-uapi.txt 2>/dev/null; then
        echo "[OK] $PATH_COUNT bond path(s) configured"
    else
        echo "[!] UAPI: $(cat /tmp/007-uapi.txt 2>/dev/null)"
    fi
fi

echo ""
echo "=== TEST 3: Ping with bond ==="
ping -c 10 -i 0.2 10.7.0.1 || true

if [ "$PATH_COUNT" -gt 1 ]; then
    echo ""
    echo "=== TEST 4: Multi-path traffic ==="
    for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
        [ "$iface" = "bond0" ] && continue
        echo -n "  $iface: "
        timeout 5 tcpdump -i "$iface" udp port 51820 -c 3 -q 2>&1 | grep -c "UDP" || echo "0"
        echo " packets"
    done &
    sleep 1
    ping -c 5 -i 0.2 10.7.0.1 > /dev/null 2>&1 || true
    wait 2>/dev/null

    echo ""
    echo "=== TEST 5: Failover ==="
    SECOND=$(ip -4 -o addr show scope global | awk '{print $2}' | sort -u | grep -v bond0 | tail -1)
    if [ -n "$SECOND" ]; then
        echo "[+] Pinging while disabling $SECOND..."
        ping -c 10 -i 0.5 10.7.0.1 > /tmp/007-failover.txt 2>&1 &
        PID=$!
        sleep 2
        echo "[+] Disabling $SECOND"
        ip link set "$SECOND" down
        sleep 3
        echo "[+] Re-enabling $SECOND"
        ip link set "$SECOND" up
        wait $PID 2>/dev/null || true
        RX=$(grep -c "bytes from" /tmp/007-failover.txt || echo 0)
        echo "[+] Result: $RX/10 pings during failover"
    fi
fi

echo ""
echo "=== TEST 6: Bond stats ==="
curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "(stats unavailable)"

echo ""
echo "=== DONE ==="
echo "Logs: /tmp/007-client.log"
echo "Stats: curl -s --max-time 3 http://127.0.0.1:8007/api/stats | python3 -m json.tool"

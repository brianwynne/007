#!/usr/bin/env bash
# 007 Bond — Server Setup (original wireguard-go, single binary)
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/007-server.sh | sudo bash
set -euo pipefail

echo "[+] Killing ALL old instances..."
pkill -9 -f '007' 2>/dev/null || true
sleep 2
rm -f /var/run/wireguard/*.sock
ip link del bond0 2>/dev/null || true
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done
# Kill anything on our ports
fuser -k 8007/tcp 2>/dev/null || true
fuser -k 51820/udp 2>/dev/null || true

echo "[+] Installing dependencies..."
apt-get update -qq && apt-get install -y -qq wireguard-tools golang-go git > /dev/null 2>&1

echo "[+] Building 007 from source..."
rm -rf /opt/007
mkdir -p /opt/007
git clone --depth 1 https://github.com/brianwynne/007.git /opt/007/src
cd /opt/007/src
go build -o /opt/007/007 .
cd /opt/007

echo "[+] Generating keys..."
wg genkey | tee server.key | wg pubkey > server.pub
wg genkey | tee client.key | wg pubkey > client.pub

echo "[+] Starting 007..."
nohup env WG_TUN_BLOCKING=1 /opt/007/007 -f bond0 > /tmp/007.log 2>&1 &
echo $! > /tmp/007.pid
sleep 3

if ! ip link show bond0 > /dev/null 2>&1; then
    echo "[X] bond0 not created. Log:"
    cat /tmp/007.log
    exit 1
fi

echo "[+] Configuring WireGuard..."
wg set bond0 listen-port 51820 private-key ./server.key \
    peer "$(cat client.pub)" allowed-ips 10.7.0.2/32
ip addr add 10.7.0.1/24 dev bond0 2>/dev/null || true
ip link set bond0 up
iptables -I INPUT -p udp --dport 51820 -j ACCEPT 2>/dev/null || true

SERVER_IP=$(hostname -I | awk '{print $1}')

echo ""
echo "========================================================"
echo "  007 SERVER RUNNING on $SERVER_IP:51820"
echo "  Kernel: $(uname -r)"
echo ""
echo "  Run on CLIENT:"
echo ""
echo "  curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/007-client.sh | sudo bash -s -- $SERVER_IP $(cat server.pub) $(cat client.key)"
echo "========================================================"
echo ""
wg show bond0
echo ""
echo "Log: tail -f /tmp/007.log"

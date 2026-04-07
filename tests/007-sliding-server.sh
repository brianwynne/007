#!/usr/bin/env bash
# 007 Bond — Sliding FEC Server Setup
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/007-sliding-server.sh -o /tmp/s.sh && sudo bash /tmp/s.sh
set -euo pipefail

export BOND_FEC_MODE=sliding

echo "[+] Cleaning up..."
for pid in $(pgrep -x 007 2>/dev/null) $(pgrep -x 007-proxy 2>/dev/null) $(pgrep -x iperf3 2>/dev/null); do kill -9 "$pid" 2>/dev/null || true; done
sleep 2
rm -f /var/run/wireguard/*.sock /opt/007/007
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done
ip link del bond0 2>/dev/null || true

echo "[+] Checking dependencies..."
for pkg in wireguard-tools golang-go git iperf3; do dpkg -s "$pkg" > /dev/null 2>&1 || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg" 2>/dev/null || true; done

echo "[+] Building 007 from source..."
cd /tmp
rm -rf /opt/007
mkdir -p /opt/007
git clone --depth 1 https://github.com/brianwynne/007.git /opt/007/src
cd /opt/007/src && go build -o /opt/007/007 .
cd /opt/007

echo "[+] Generating keys..."
wg genkey | tee server.key | wg pubkey > server.pub
wg genkey | tee client.key | wg pubkey > client.pub

echo "[+] Starting 007 with SLIDING FEC..."
nohup env BOND_FEC_MODE=sliding BOND_API=0.0.0.0:8007 /opt/007/007 -f bond0 > /tmp/007.log 2>&1 &
echo $! > /tmp/007.pid
sleep 3

if ! ip link show bond0 > /dev/null 2>&1; then echo "[X] bond0 not created:"; cat /tmp/007.log; exit 1; fi

echo "[+] Configuring WireGuard..."
wg set bond0 listen-port 51820 private-key ./server.key peer "$(cat client.pub)" allowed-ips 10.7.0.2/32
ip addr add 10.7.0.1/24 dev bond0 2>/dev/null || true
ip link set bond0 up
iptables -I INPUT -p udp --dport 51820 -j ACCEPT 2>/dev/null || true
iptables -I INPUT -p tcp --dport 8007 -s 10.7.0.0/24 -j ACCEPT 2>/dev/null || true

pkill -x iperf3 2>/dev/null || true
nohup bash -c 'while true; do iperf3 -s -B 10.7.0.1 --one-off 2>/dev/null; sleep 1; done' > /tmp/iperf3.log 2>&1 &

echo "[+] Verifying sliding FEC:"
grep -i "sliding\|fec_mode\|FEC" /tmp/007.log | head -3

SERVER_IP=$(hostname -I | awk '{print $1}')
echo ""
echo "========================================================"
echo "  007 SERVER (SLIDING FEC) on $SERVER_IP:51820"
echo ""
echo "  Run on CLIENT:"
echo ""
echo "  curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/007-sliding-client.sh -o /tmp/c.sh && sudo bash /tmp/c.sh $SERVER_IP $(cat server.pub) $(cat client.key)"
echo "========================================================"

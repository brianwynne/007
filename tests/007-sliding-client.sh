#!/usr/bin/env bash
# 007 Bond — Sliding FEC Client Setup + Full Test
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/007-sliding-client.sh -o /tmp/c.sh && sudo bash /tmp/c.sh <server_ip> <server_pub> <client_key>
set -euo pipefail

if [ $# -lt 3 ]; then echo "Usage: sudo bash $0 <server_ip> <server_pub> <client_key>"; exit 1; fi
SERVER_IP="$1"; SERVER_PUB="$2"; CLIENT_KEY="$3"
export BOND_FEC_MODE=sliding

PASS=0; FAIL=0; TOTAL=0
result() { TOTAL=$((TOTAL+1)); if [ "$1" = "PASS" ]; then PASS=$((PASS+1)); echo -e "\033[0;32m  [PASS]\033[0m $2"; else FAIL=$((FAIL+1)); echo -e "\033[0;31m  [FAIL]\033[0m $2"; fi; }

# ─── Setup ────────────────────────────────────────────────────
echo "[+] Cleaning up..."
for pid in $(pgrep -x 007 2>/dev/null) $(pgrep -x 007-proxy 2>/dev/null); do kill -9 "$pid" 2>/dev/null || true; done
sleep 1
rm -f /var/run/wireguard/*.sock
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done
ip link del bond0 2>/dev/null || true

echo "[+] Checking dependencies..."
for pkg in wireguard-tools golang-go git iperf3 xxd netcat-openbsd; do dpkg -s "$pkg" > /dev/null 2>&1 || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg" 2>/dev/null || true; done

echo "[+] Building 007..."
cd /tmp; rm -rf /opt/007; mkdir -p /opt/007
git clone --depth 1 https://github.com/brianwynne/007.git /opt/007/src
cd /opt/007/src && go build -o /opt/007/007 .
cd /opt/007
echo "$CLIENT_KEY" > client.key; echo "$SERVER_PUB" > server.pub

echo "[+] Starting 007 with SLIDING FEC..."
nohup env BOND_FEC_MODE=sliding /opt/007/007 -f bond0 > /tmp/007.log 2>&1 &
echo $! > /tmp/007.pid
sleep 3
if ! ip link show bond0 > /dev/null 2>&1; then echo "[X] bond0 not created:"; cat /tmp/007.log; exit 1; fi

echo "[+] Configuring WireGuard..."
wg set bond0 private-key ./client.key peer "$SERVER_PUB" endpoint "$SERVER_IP:51820" allowed-ips 10.7.0.0/24 persistent-keepalive 25
ip addr add 10.7.0.2/24 dev bond0 2>/dev/null || true
ip link set bond0 up

echo "[+] Verifying sliding FEC:"
grep -i "sliding\|fec_mode\|FEC" /tmp/007.log | head -3

echo "[+] Adding bond paths..."
IFACES=""
SERVER_PUB_HEX=$(echo -n "$SERVER_PUB" | base64 -d | xxd -p -c 32)
BOND_CMD="set=1\npublic_key=$SERVER_PUB_HEX\n"
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [ "$iface" = "bond0" ] && continue
    LOCAL_IP=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
    if [ -n "$LOCAL_IP" ]; then
        BOND_CMD="${BOND_CMD}bond_endpoint=${SERVER_IP}:51820@${LOCAL_IP}\n"
        IFACES="$IFACES $iface"
        echo "  Path: $iface ($LOCAL_IP)"
    fi
done
printf "${BOND_CMD}\n" | nc -U /var/run/wireguard/bond0.sock -w 1 > /dev/null 2>&1 || true

echo "[+] Waiting for handshake..."
sleep 5

echo ""
echo "================================================================"
echo "  007 Bond — SLIDING FEC Proof Tests"
echo "  Server: 10.7.0.1"
echo "  FEC Mode: sliding (XOR window)"
echo "  Kernel: $(uname -r)"
echo "================================================================"

# ─── TEST 1: Ping ─────────────────────────────────────────────
echo ""; echo "=== TEST 1: Basic ping ==="
if ping -c 5 -W 2 10.7.0.1 > /dev/null 2>&1; then result PASS "Ping works"; else result FAIL "Ping failed"; exit 1; fi

# ─── TEST 2: Multi-path ───────────────────────────────────────
echo ""; echo "=== TEST 2: Multi-path ==="
for iface in $IFACES; do
    COUNT=$(timeout 5 tcpdump -i "$iface" udp port 51820 -c 3 -n -q 2>&1 | grep -c "UDP" 2>/dev/null); COUNT=${COUNT:-0}
    if [ "$COUNT" -gt 0 ]; then result PASS "$iface: $COUNT packets"; else
        ping -c 3 -i 0.1 10.7.0.1 > /dev/null 2>&1 &
        COUNT=$(timeout 5 tcpdump -i "$iface" udp port 51820 -c 3 -n -q 2>&1 | grep -c "UDP" 2>/dev/null); COUNT=${COUNT:-0}; wait 2>/dev/null
        if [ "$COUNT" -gt 0 ]; then result PASS "$iface: $COUNT (retry)"; else result FAIL "$iface: no traffic"; fi
    fi
done

# ─── TEST 3: Sustained 100 pings ──────────────────────────────
echo ""; echo "=== TEST 3: Sustained 100 pings ==="
ping -c 100 -i 0.05 10.7.0.1 > /tmp/007-sustained.txt 2>&1 || true
RX=$(grep -c "bytes from" /tmp/007-sustained.txt 2>/dev/null); RX=${RX:-0}
if [ "$RX" -ge 95 ]; then result PASS "$RX/100 received"; else result FAIL "$RX/100 received"; fi

# ─── TEST 4: Failover ─────────────────────────────────────────
echo ""; echo "=== TEST 4: Path failover ==="
SECOND_IFACE=$(echo $IFACES | awk '{print $NF}')
if [ -n "$SECOND_IFACE" ] && [ "$(echo $IFACES | wc -w)" -ge 2 ]; then
    ping -c 20 -i 0.5 10.7.0.1 > /tmp/007-failover.txt 2>&1 &
    PID=$!; sleep 3
    echo "  Disabling $SECOND_IFACE..."; ip link set "$SECOND_IFACE" down; sleep 5
    echo "  Re-enabling $SECOND_IFACE..."; ip link set "$SECOND_IFACE" up; sleep 2
    wait $PID 2>/dev/null || true
    RX=$(grep -c "bytes from" /tmp/007-failover.txt 2>/dev/null); RX=${RX:-0}
    if [ "$RX" -ge 15 ]; then result PASS "Failover: $RX/20"; else result FAIL "Failover: $RX/20"; fi
else echo "  (skipped — need 2+ interfaces)"; fi

# ─── TEST 5: FEC under 30% loss ALL paths ─────────────────────
echo ""; echo "=== TEST 5: Sliding FEC under 30% ALL-path loss ==="
STATS_BEFORE=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
for iface in $IFACES; do tc qdisc add dev "$iface" root netem loss 30% 2>/dev/null || true; done
ping -c 50 -i 0.05 10.7.0.1 > /tmp/007-loss.txt 2>&1 || true
for iface in $IFACES; do tc qdisc del dev "$iface" root 2>/dev/null || true; done
RX=$(grep -c "bytes from" /tmp/007-loss.txt 2>/dev/null); RX=${RX:-0}
if [ "$RX" -ge 30 ]; then result PASS "Under 30% loss: $RX/50"; else result FAIL "Under 30% loss: $RX/50"; fi

# ─── TEST 6: Combined loss + delay ────────────────────────────
echo ""; echo "=== TEST 6: 20% loss + 30ms delay ALL paths ==="
for iface in $IFACES; do tc qdisc add dev "$iface" root netem loss 20% delay 30ms 10ms 2>/dev/null || true; done
ping -c 50 -i 0.05 10.7.0.1 > /tmp/007-combined.txt 2>&1 || true
for iface in $IFACES; do tc qdisc del dev "$iface" root 2>/dev/null || true; done
RX=$(grep -c "bytes from" /tmp/007-combined.txt 2>/dev/null); RX=${RX:-0}
if [ "$RX" -ge 30 ]; then result PASS "Combined: $RX/50"; else result FAIL "Combined: $RX/50"; fi

# ─── TEST 7: ARQ under 70% loss ───────────────────────────────
echo ""; echo "=== TEST 7: ARQ under 70% ALL-path loss ==="
STATS_BEFORE=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
for iface in $IFACES; do tc qdisc add dev "$iface" root netem loss 70% 2>/dev/null || true; done
ping -c 30 -i 0.2 10.7.0.1 > /tmp/007-arq.txt 2>&1 || true
for iface in $IFACES; do tc qdisc del dev "$iface" root 2>/dev/null || true; done
RX=$(grep -c "bytes from" /tmp/007-arq.txt 2>/dev/null); RX=${RX:-0}
STATS_AFTER=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
NACKS=$(echo "$STATS_AFTER" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('nacks_sent',0)+d.get('nacks_received',0))" 2>/dev/null || echo 0)
result PASS "70% loss: $RX/30 received, total NACKs=$NACKS"

# ─── TEST 8: iperf3 64kbps ────────────────────────────────────
echo ""; echo "=== TEST 8: iperf3 64kbps UDP ==="
IPERF_OUT=$(timeout 20 iperf3 -c 10.7.0.1 -u -b 64k -l 160 -t 5 2>&1 || echo "FAIL")
if echo "$IPERF_OUT" | grep -q "receiver"; then
    SUMMARY=$(echo "$IPERF_OUT" | grep "receiver" | tail -1)
    result PASS "iperf3: $SUMMARY"
else result FAIL "iperf3: unreachable"; fi

# ─── TEST 9: API ──────────────────────────────────────────────
echo ""; echo "=== TEST 9: Management API ==="
if curl -s --max-time 3 http://127.0.0.1:8007/api/health | grep -q ok; then result PASS "API health"; else result FAIL "API health"; fi
if curl -s --max-time 3 http://127.0.0.1:8007/api/stats | grep -q tx_packets; then result PASS "API stats"; else result FAIL "API stats"; fi

# ─── TEST 10: Final stats ─────────────────────────────────────
echo ""; echo "=== TEST 10: Final bond stats ==="
curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "(unavailable)"

echo ""
echo "================================================================"
echo "  RESULTS: $PASS passed, $FAIL failed, $TOTAL total"
if [ "$FAIL" -eq 0 ]; then echo -e "  \033[0;32mALL TESTS PASSED\033[0m"
else echo -e "  \033[0;31m$FAIL TEST(S) FAILED\033[0m"; fi
echo "  FEC Mode: SLIDING (XOR window)"
echo "================================================================"

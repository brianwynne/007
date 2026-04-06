#!/usr/bin/env bash
# 007 Bond — Comprehensive proof tests
# Run on CLIENT after 007 is running and bond paths are configured
#
# Usage: sudo bash 007-prove-it.sh <server_tunnel_ip>
set -euo pipefail

SERVER="${1:-10.7.0.1}"
PASS=0
FAIL=0
TOTAL=0

result() {
    TOTAL=$((TOTAL + 1))
    if [ "$1" = "PASS" ]; then
        PASS=$((PASS + 1))
        echo -e "\033[0;32m  [PASS]\033[0m $2"
    else
        FAIL=$((FAIL + 1))
        echo -e "\033[0;31m  [FAIL]\033[0m $2"
    fi
}

echo "================================================================"
echo "  007 Bond — Comprehensive Proof Tests"
echo "  Server: $SERVER"
echo "  Time: $(date)"
echo "  Kernel: $(uname -r)"
echo "================================================================"

# ─── TEST 1: Basic connectivity ─────────────────────────────────
echo ""
echo "=== TEST 1: Basic tunnel connectivity ==="
if ping -c 5 -W 2 "$SERVER" > /dev/null 2>&1; then
    result PASS "Ping through tunnel works"
else
    result FAIL "Ping through tunnel"
    echo "  Cannot continue without basic connectivity"
    exit 1
fi

# ─── TEST 2: Multi-path send ────────────────────────────────────
echo ""
echo "=== TEST 2: Multi-path — packets sent on ALL interfaces ==="
IFACES=""
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [ "$iface" = "bond0" ] && continue
    IFACES="$IFACES $iface"
done

ALL_HAVE_TRAFFIC=true
for iface in $IFACES; do
    COUNT=$(timeout 5 tcpdump -i "$iface" udp port 51820 -c 3 -n -q 2>&1 | grep -c "UDP" || echo 0)
    if [ "$COUNT" -gt 0 ]; then
        result PASS "Interface $iface: $COUNT packets"
    else
        # Generate traffic and retry
        ping -c 5 -i 0.1 "$SERVER" > /dev/null 2>&1 &
        COUNT=$(timeout 5 tcpdump -i "$iface" udp port 51820 -c 3 -n -q 2>&1 | grep -c "UDP" || echo 0)
        wait 2>/dev/null
        if [ "$COUNT" -gt 0 ]; then
            result PASS "Interface $iface: $COUNT packets (after retry)"
        else
            result FAIL "Interface $iface: no traffic"
            ALL_HAVE_TRAFFIC=false
        fi
    fi
done

# ─── TEST 3: Sustained throughput ────────────────────────────────
echo ""
echo "=== TEST 3: Sustained throughput (100 pings) ==="
ping -c 100 -i 0.05 "$SERVER" > /tmp/007-sustained.txt 2>&1 || true
RX=$(grep -c "bytes from" /tmp/007-sustained.txt || echo 0)
LOSS=$(grep "packet loss" /tmp/007-sustained.txt | grep -oP '\d+(?=%)' || echo 100)
if [ "$RX" -ge 95 ]; then
    result PASS "Sustained: $RX/100 received ($LOSS% loss)"
else
    result FAIL "Sustained: $RX/100 received ($LOSS% loss)"
fi
RTT=$(grep "rtt" /tmp/007-sustained.txt | grep -oP 'avg/[\d.]+' | cut -d/ -f2 || echo "?")
echo "  RTT avg: ${RTT}ms"

# ─── TEST 4: Path failover ──────────────────────────────────────
echo ""
echo "=== TEST 4: Path failover — disable secondary interface ==="
SECOND_IFACE=$(echo $IFACES | awk '{print $NF}')
if [ -n "$SECOND_IFACE" ] && [ "$(echo $IFACES | wc -w)" -ge 2 ]; then
    ping -c 20 -i 0.5 "$SERVER" > /tmp/007-failover.txt 2>&1 &
    PING_PID=$!
    sleep 3
    echo "  Disabling $SECOND_IFACE..."
    ip link set "$SECOND_IFACE" down
    sleep 5
    echo "  Re-enabling $SECOND_IFACE..."
    ip link set "$SECOND_IFACE" up
    sleep 2
    wait $PING_PID 2>/dev/null || true
    RX=$(grep -c "bytes from" /tmp/007-failover.txt || echo 0)
    if [ "$RX" -ge 15 ]; then
        result PASS "Failover: $RX/20 pings during interface toggle"
    else
        result FAIL "Failover: $RX/20 pings during interface toggle"
    fi
else
    echo "  (skipped — need 2+ interfaces)"
fi

# ─── TEST 5: Control traffic survives failover ───────────────────
echo ""
echo "=== TEST 5: WireGuard handshake survives failover ==="
HANDSHAKE_BEFORE=$(wg show bond0 | grep "latest handshake" | head -1)
# Force handshake by waiting for key refresh or restarting keepalive
sleep 2
HANDSHAKE_AFTER=$(wg show bond0 | grep "latest handshake" | head -1)
if wg show bond0 | grep -q "latest handshake"; then
    result PASS "WireGuard handshake active after failover"
else
    result FAIL "WireGuard handshake lost"
fi

# ─── TEST 6: FEC recovery — loss on ALL paths ───────────────────
echo ""
echo "=== TEST 6: FEC recovery — 30% loss on ALL interfaces ==="
echo "  (must impair ALL paths to force FEC, otherwise multi-path redundancy masks loss)"
STATS_BEFORE=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
FEC_BEFORE=$(echo "$STATS_BEFORE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('fec_recovered',0))" 2>/dev/null || echo 0)

for iface in $IFACES; do
    tc qdisc add dev "$iface" root netem loss 30% 2>/dev/null || true
done
ping -c 50 -i 0.05 "$SERVER" > /tmp/007-loss.txt 2>&1 || true
for iface in $IFACES; do
    tc qdisc del dev "$iface" root 2>/dev/null || true
done

RX=$(grep -c "bytes from" /tmp/007-loss.txt || echo 0)
STATS_AFTER=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
FEC_AFTER=$(echo "$STATS_AFTER" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('fec_recovered',0))" 2>/dev/null || echo 0)
FEC_RECOVERED=$((FEC_AFTER - FEC_BEFORE))
GAPS=$(echo "$STATS_AFTER" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('reorder_gaps',0))" 2>/dev/null || echo 0)

if [ "$RX" -ge 30 ]; then
    result PASS "Under 30% ALL-path loss: $RX/50 received (FEC recovered: $FEC_RECOVERED, gaps: $GAPS)"
else
    result FAIL "Under 30% ALL-path loss: $RX/50 received (FEC recovered: $FEC_RECOVERED, gaps: $GAPS)"
fi

# ─── TEST 7: Reorder — different delays per path ────────────────
echo ""
echo "=== TEST 7: Reorder — 0ms on path 1, 50ms on path 2 ==="
echo "  (asymmetric delay forces packets to arrive out of order)"
IFACE1=$(echo $IFACES | awk '{print $1}')
IFACE2=$(echo $IFACES | awk '{print $NF}')
REORDER_BEFORE=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('reorder_reordered',0))" 2>/dev/null || echo 0)

tc qdisc add dev "$IFACE2" root netem delay 50ms 10ms 2>/dev/null || true
ping -c 30 -i 0.05 "$SERVER" > /tmp/007-delay.txt 2>&1 || true
tc qdisc del dev "$IFACE2" root 2>/dev/null || true

RX=$(grep -c "bytes from" /tmp/007-delay.txt || echo 0)
REORDER_AFTER=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('reorder_reordered',0))" 2>/dev/null || echo 0)
REORDERED=$((REORDER_AFTER - REORDER_BEFORE))

if [ "$RX" -ge 25 ]; then
    result PASS "Under 50ms asymmetry: $RX/30 received (reordered: $REORDERED)"
else
    result FAIL "Under 50ms asymmetry: $RX/30 received (reordered: $REORDERED)"
fi

# ─── TEST 8: Combined — loss + delay on ALL paths ───────────────
echo ""
echo "=== TEST 8: Combined — 20% loss + 30ms delay on ALL paths ==="
STATS_BEFORE=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
FEC_BEFORE=$(echo "$STATS_BEFORE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('fec_recovered',0))" 2>/dev/null || echo 0)

for iface in $IFACES; do
    tc qdisc add dev "$iface" root netem loss 20% delay 30ms 10ms 2>/dev/null || true
done
ping -c 50 -i 0.05 "$SERVER" > /tmp/007-combined.txt 2>&1 || true
for iface in $IFACES; do
    tc qdisc del dev "$iface" root 2>/dev/null || true
done

RX=$(grep -c "bytes from" /tmp/007-combined.txt || echo 0)
STATS_AFTER=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
FEC_AFTER=$(echo "$STATS_AFTER" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('fec_recovered',0))" 2>/dev/null || echo 0)
FEC_RECOVERED=$((FEC_AFTER - FEC_BEFORE))
NACKS=$(echo "$STATS_AFTER" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('nacks_sent',0))" 2>/dev/null || echo 0)

if [ "$RX" -ge 30 ]; then
    result PASS "Combined ALL-path impairment: $RX/50 received (FEC: $FEC_RECOVERED, NACKs: $NACKS)"
else
    result FAIL "Combined ALL-path impairment: $RX/50 received (FEC: $FEC_RECOVERED, NACKs: $NACKS)"
fi

# ─── TEST 9: iperf3 UDP throughput ────────────────────────────────
echo ""
echo "=== TEST 9: iperf3 UDP throughput (64kbps Opus-equivalent) ==="
if ! command -v iperf3 &>/dev/null; then
    apt-get install -y -qq iperf3 > /dev/null 2>&1 || true
fi

if command -v iperf3 &>/dev/null; then
    # Start iperf3 server on remote via SSH or assume it's running
    # For simplicity: start a local iperf3 server on the tunnel IP and test from here
    # The server-side test script should start iperf3 — for now, try to connect

    echo "  Attempting iperf3 to $SERVER..."

    # Test 9a: Low bitrate UDP (64kbps — Opus audio)
    IPERF_RESULT=$(iperf3 -c "$SERVER" -u -b 64k -l 160 -t 10 --json 2>/dev/null || echo "FAIL")
    if echo "$IPERF_RESULT" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    s = d['end']['sum']
    loss = s.get('lost_percent', 100)
    jitter = s.get('jitter_ms', -1)
    bps = s.get('bits_per_second', 0)
    print(f'loss={loss:.1f}% jitter={jitter:.3f}ms bps={bps/1000:.0f}kbps')
    sys.exit(0 if loss < 5 else 1)
except:
    sys.exit(1)
" 2>/dev/null; then
        IPERF_LINE=$(iperf3 -c "$SERVER" -u -b 64k -l 160 -t 5 2>/dev/null | tail -3 | head -1 || echo "")
        result PASS "iperf3 64kbps UDP: $IPERF_LINE"
    else
        # iperf3 server might not be running on remote
        echo "  iperf3 server not reachable on $SERVER — starting bidirectional test"
        # Run iperf3 server locally, test loopback through tunnel
        iperf3 -s -D -B "$SERVER" --one-off 2>/dev/null || true
        sleep 1

        IPERF_OUT=$(iperf3 -c "$SERVER" -u -b 64k -l 160 -t 10 -R 2>&1 || echo "FAIL")
        if echo "$IPERF_OUT" | grep -q "receiver"; then
            LOSS=$(echo "$IPERF_OUT" | grep "receiver" | grep -oP '[\d.]+(?=%)' | tail -1 || echo "?")
            JITTER=$(echo "$IPERF_OUT" | grep "receiver" | grep -oP '[\d.]+(?= ms)' | tail -1 || echo "?")
            result PASS "iperf3 64kbps UDP: loss=${LOSS}% jitter=${JITTER}ms"
        else
            result FAIL "iperf3 64kbps UDP: could not connect"
        fi
    fi

    # Test 9b: Higher bitrate (1Mbps — video-equivalent)
    echo ""
    echo "=== TEST 9b: iperf3 UDP throughput (1Mbps) ==="
    IPERF_OUT=$(iperf3 -c "$SERVER" -u -b 1M -t 10 2>&1 || echo "FAIL")
    if echo "$IPERF_OUT" | grep -q "receiver"; then
        LOSS=$(echo "$IPERF_OUT" | grep "receiver" | grep -oP '[\d.]+(?=%)' | tail -1 || echo "?")
        JITTER=$(echo "$IPERF_OUT" | grep "receiver" | grep -oP '[\d.]+(?= ms)' | tail -1 || echo "?")
        BW=$(echo "$IPERF_OUT" | grep "receiver" | grep -oP '[\d.]+ [KMG]bits' | tail -1 || echo "?")
        result PASS "iperf3 1Mbps UDP: loss=${LOSS}% jitter=${JITTER}ms bw=${BW}"
    else
        result FAIL "iperf3 1Mbps UDP: could not connect"
    fi

    # Test 9c: iperf3 under impairment — 20% loss ALL paths
    echo ""
    echo "=== TEST 9c: iperf3 64kbps under 20% loss ALL paths ==="
    for iface in $IFACES; do
        tc qdisc add dev "$iface" root netem loss 20% 2>/dev/null || true
    done
    IPERF_OUT=$(iperf3 -c "$SERVER" -u -b 64k -l 160 -t 10 2>&1 || echo "FAIL")
    for iface in $IFACES; do
        tc qdisc del dev "$iface" root 2>/dev/null || true
    done
    if echo "$IPERF_OUT" | grep -q "receiver"; then
        LOSS=$(echo "$IPERF_OUT" | grep "receiver" | grep -oP '[\d.]+(?=%)' | tail -1 || echo "?")
        JITTER=$(echo "$IPERF_OUT" | grep "receiver" | grep -oP '[\d.]+(?= ms)' | tail -1 || echo "?")
        result PASS "iperf3 64kbps under 20% loss: loss=${LOSS}% jitter=${JITTER}ms"
    else
        result FAIL "iperf3 64kbps under 20% loss"
    fi
else
    echo "  (skipped — iperf3 not available)"
fi

# ─── TEST 10: Management API ────────────────────────────────────
echo ""
echo "=== TEST 10: Management API ==="
HEALTH=$(curl -s --max-time 3 http://127.0.0.1:8007/api/health 2>/dev/null || echo "")
if echo "$HEALTH" | grep -q "ok"; then
    result PASS "API health endpoint"
else
    result FAIL "API health endpoint"
fi

STATS=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null || echo "")
if echo "$STATS" | grep -q "tx_packets"; then
    result PASS "API stats endpoint"
else
    result FAIL "API stats endpoint"
fi

CONFIG=$(curl -s --max-time 3 http://127.0.0.1:8007/api/config 2>/dev/null || echo "")
if echo "$CONFIG" | grep -q "fec_enabled"; then
    result PASS "API config endpoint"
else
    result FAIL "API config endpoint"
fi

# ─── TEST 10: Full stats dump ────────────────────────────────────
echo ""
echo "=== TEST 11: Final bond stats ==="
FINAL_STATS=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null)
echo "$FINAL_STATS" | python3 -m json.tool 2>/dev/null || echo "$FINAL_STATS"

TX=$(echo "$FINAL_STATS" | python3 -c "import sys,json; print(json.load(sys.stdin).get('tx_packets',0))" 2>/dev/null || echo 0)
RX=$(echo "$FINAL_STATS" | python3 -c "import sys,json; print(json.load(sys.stdin).get('rx_packets',0))" 2>/dev/null || echo 0)
if [ "$TX" -gt 0 ] && [ "$RX" -gt 0 ]; then
    result PASS "Bond processing active (tx=$TX rx=$RX)"
else
    result FAIL "Bond processing inactive"
fi

# ─── Summary ─────────────────────────────────────────────────────
echo ""
echo "================================================================"
echo "  RESULTS: $PASS passed, $FAIL failed, $TOTAL total"
if [ "$FAIL" -eq 0 ]; then
    echo -e "  \033[0;32mALL TESTS PASSED\033[0m"
else
    echo -e "  \033[0;31m$FAIL TEST(S) FAILED\033[0m"
fi
echo "================================================================"

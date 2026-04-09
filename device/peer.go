/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"container/list"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

type Peer struct {
	isRunning         atomic.Bool
	keypairs          Keypairs
	handshake         Handshake
	device            *Device
	stopping          sync.WaitGroup // routines pending stop
	txBytes           atomic.Uint64  // bytes send to peer (endpoint)
	rxBytes           atomic.Uint64  // bytes received from peer
	lastHandshakeNano atomic.Int64   // nano seconds since epoch

	endpoint struct {
		sync.Mutex
		val            conn.Endpoint // primary endpoint (handshake, roaming)
		bondPaths      []BondPath    // multi-path send via per-interface sockets
		clearSrcOnTx   bool          // signal to val.ClearSrc() prior to next packet transmission
		disableRoaming bool

		// Auto-discovered remote endpoints for multi-path send.
		// When a peer sends from multiple source IPs (e.g. eth0 + wlan0),
		// we track all of them and send replies to ALL — enabling seamless
		// failover without requiring UAPI bond_endpoint configuration.
		discovered     map[string]discoveredEndpoint // key = DstToString()
	}


	timers struct {
		retransmitHandshake     *Timer
		sendKeepalive           *Timer
		newHandshake            *Timer
		zeroKeyMaterial         *Timer
		persistentKeepalive     *Timer
		handshakeAttempts       atomic.Uint32
		needAnotherKeepalive    atomic.Bool
		sentLastMinuteHandshake atomic.Bool
	}

	state struct {
		sync.Mutex // protects against concurrent Start/Stop
	}

	queue struct {
		staged   chan *QueueOutboundElementsContainer // staged packets before a handshake is available
		outbound *autodrainingOutboundQueue           // sequential ordering of udp transmission
		inbound  *autodrainingInboundQueue            // sequential ordering of tun writing
	}

	cookieGenerator             CookieGenerator
	trieEntries                 list.List
	persistentKeepaliveInterval atomic.Uint32
	bondPeerID                  uint32 // unique ID for per-peer FEC state in bond manager
}

func (device *Device) NewPeer(pk NoisePublicKey) (*Peer, error) {
	if device.isClosed() {
		return nil, errors.New("device closed")
	}

	// lock resources
	device.staticIdentity.RLock()
	defer device.staticIdentity.RUnlock()

	device.peers.Lock()
	defer device.peers.Unlock()

	// check if over limit
	if len(device.peers.keyMap) >= MaxPeers {
		return nil, errors.New("too many peers")
	}

	// create peer
	peer := new(Peer)
	peer.bondPeerID = device.nextBondPeerID.Add(1)

	peer.cookieGenerator.Init(pk)
	peer.device = device
	peer.queue.outbound = newAutodrainingOutboundQueue(device)
	peer.queue.inbound = newAutodrainingInboundQueue(device)
	peer.queue.staged = make(chan *QueueOutboundElementsContainer, QueueStagedSize)

	// map public key
	_, ok := device.peers.keyMap[pk]
	if ok {
		return nil, errors.New("adding existing peer")
	}

	// pre-compute DH
	handshake := &peer.handshake
	handshake.mutex.Lock()
	handshake.precomputedStaticStatic, _ = device.staticIdentity.privateKey.sharedSecret(pk)
	handshake.remoteStatic = pk
	handshake.mutex.Unlock()

	// reset endpoint
	peer.endpoint.Lock()
	peer.endpoint.val = nil
	peer.endpoint.disableRoaming = false
	peer.endpoint.clearSrcOnTx = false
	peer.endpoint.Unlock()

	// init timers
	peer.timersInit()

	// add
	device.peers.keyMap[pk] = peer

	return peer, nil
}

// BondPath represents a multi-path send channel with its own UDP socket
// bound to a specific local IP address, ensuring traffic exits via
// the correct physical interface.
type BondPath struct {
	Endpoint conn.Endpoint  // destination address
	LocalIP  netip.Addr     // local IP this socket is bound to
	conn     *net.UDPConn   // dedicated socket bound to LocalIP
	dst      *net.UDPAddr   // cached parsed destination (avoids per-packet parsing)
}

// Send transmits buffers via this path's dedicated socket.
func (bp *BondPath) Send(buffers [][]byte) error {
	if bp.conn == nil {
		return errors.New("bond path socket not open")
	}
	for _, buf := range buffers {
		_, err := bp.conn.WriteToUDP(buf, bp.dst)
		if err != nil {
			return err
		}
	}
	return nil
}

// Close closes the path's dedicated socket.
func (bp *BondPath) Close() error {
	if bp.conn != nil {
		return bp.conn.Close()
	}
	return nil
}

func (peer *Peer) SendBuffers(buffers [][]byte) error {
	peer.device.net.RLock()
	defer peer.device.net.RUnlock()

	if peer.device.isClosed() {
		return nil
	}

	peer.endpoint.Lock()
	endpoint := peer.endpoint.val
	if endpoint == nil {
		peer.endpoint.Unlock()
		return errors.New("no known endpoint for peer")
	}
	if peer.endpoint.clearSrcOnTx {
		endpoint.ClearSrc()
		peer.endpoint.clearSrcOnTx = false
	}
	// Snapshot bond paths under lock
	bondPaths := peer.endpoint.bondPaths
	// Snapshot discovered endpoints under lock
	var discoveredEPs []conn.Endpoint
	for _, de := range peer.endpoint.discovered {
		discoveredEPs = append(discoveredEPs, de.endpoint)
	}
	peer.endpoint.Unlock()

	var err error

	if len(bondPaths) == 0 {
		// Server side — send via standard bind.
		if len(discoveredEPs) > 0 {
			// Discovered endpoints exist — send to ALL of them ONLY.
			// Skip endpoint.val (primary) as it may be a stale handshake
			// address. The discovered map contains only actively seen
			// endpoints with 60-second expiry.
			for _, ep := range discoveredEPs {
				peer.device.net.bind.Send(buffers, ep)
			}
		} else {
			// No discovered endpoints yet — use primary only
			// (initial handshake, before any data packets arrive)
			err = peer.device.net.bind.Send(buffers, endpoint)
			if err == nil {
				var totalLen uint64
				for _, b := range buffers {
					totalLen += uint64(len(b))
				}
				peer.txBytes.Add(totalLen)
			}
		}
	} else {
		// Bond paths configured (client side) — send via dedicated
		// per-interface sockets ONLY. No primary bind send — that would
		// be a redundant third copy via kernel-picked interface.
		for i := range bondPaths {
			bondPaths[i].Send(buffers)
		}
	}

	return err
}

// AddBondPath adds a multi-path send channel bound to a specific local IP.
// The localIP determines which physical interface the traffic exits through.
// The dest endpoint is the remote address to send to on this path.
func (peer *Peer) AddBondPath(dest conn.Endpoint, localIP netip.Addr) error {
	// Idempotent: if a bond path with this local IP already exists, skip.
	// This allows the management script to always add without clearing,
	// preserving existing sockets and NAT pinholes.
	peer.endpoint.Lock()
	for _, bp := range peer.endpoint.bondPaths {
		if bp.LocalIP == localIP {
			peer.endpoint.Unlock()
			return nil // already exists
		}
	}
	peer.endpoint.Unlock()

	// Create UDP socket bound to the local IP
	var network string
	var localAddr *net.UDPAddr
	if localIP.Is4() {
		network = "udp4"
		localAddr = &net.UDPAddr{IP: localIP.AsSlice(), Port: 0}
	} else {
		network = "udp6"
		localAddr = &net.UDPAddr{IP: localIP.AsSlice(), Port: 0}
	}

	udpConn, err := net.ListenUDP(network, localAddr)
	if err != nil {
		return err
	}

	// Cache parsed destination address to avoid per-packet string parsing
	dstAddrPort, err := netip.ParseAddrPort(dest.DstToString())
	if err != nil {
		udpConn.Close()
		return err
	}

	peer.endpoint.Lock()
	peer.endpoint.bondPaths = append(peer.endpoint.bondPaths, BondPath{
		Endpoint: dest,
		LocalIP:  localIP,
		conn:     udpConn,
		dst:      net.UDPAddrFromAddrPort(dstAddrPort),
	})
	peer.endpoint.Unlock()

	// Start receive goroutine — bond paths must be bidirectional.
	// Without this, server endpoint roaming sends replies to bond path
	// sockets that nobody reads, breaking the return path.
	peer.device.StartBondPathReceiver(udpConn)

	return nil
}

// ClearBondPaths closes and removes all bond paths.
func (peer *Peer) ClearBondPaths() {
	peer.endpoint.Lock()
	defer peer.endpoint.Unlock()
	for i := range peer.endpoint.bondPaths {
		peer.endpoint.bondPaths[i].Close()
	}
	peer.endpoint.bondPaths = nil
}

// BondPathCount returns the number of active bond paths.
func (peer *Peer) BondPathCount() int {
	peer.endpoint.Lock()
	defer peer.endpoint.Unlock()
	return len(peer.endpoint.bondPaths)
}

func (peer *Peer) String() string {
	// The awful goo that follows is identical to:
	//
	//   base64Key := base64.StdEncoding.EncodeToString(peer.handshake.remoteStatic[:])
	//   abbreviatedKey := base64Key[0:4] + "…" + base64Key[39:43]
	//   return fmt.Sprintf("peer(%s)", abbreviatedKey)
	//
	// except that it is considerably more efficient.
	src := peer.handshake.remoteStatic
	b64 := func(input byte) byte {
		return input + 'A' + byte(((25-int(input))>>8)&6) - byte(((51-int(input))>>8)&75) - byte(((61-int(input))>>8)&15) + byte(((62-int(input))>>8)&3)
	}
	b := []byte("peer(____…____)")
	const first = len("peer(")
	const second = len("peer(____…")
	b[first+0] = b64((src[0] >> 2) & 63)
	b[first+1] = b64(((src[0] << 4) | (src[1] >> 4)) & 63)
	b[first+2] = b64(((src[1] << 2) | (src[2] >> 6)) & 63)
	b[first+3] = b64(src[2] & 63)
	b[second+0] = b64(src[29] & 63)
	b[second+1] = b64((src[30] >> 2) & 63)
	b[second+2] = b64(((src[30] << 4) | (src[31] >> 4)) & 63)
	b[second+3] = b64((src[31] << 2) & 63)
	return string(b)
}

func (peer *Peer) Start() {
	// should never start a peer on a closed device
	if peer.device.isClosed() {
		return
	}

	// prevent simultaneous start/stop operations
	peer.state.Lock()
	defer peer.state.Unlock()

	if peer.isRunning.Load() {
		return
	}

	device := peer.device
	device.log.Verbosef("%v - Starting", peer)

	// reset routine state
	peer.stopping.Wait()
	peer.stopping.Add(2)

	peer.handshake.mutex.Lock()
	peer.handshake.lastSentHandshake = time.Now().Add(-(RekeyTimeout + time.Second))
	peer.handshake.mutex.Unlock()

	peer.device.queue.encryption.wg.Add(1) // keep encryption queue open for our writes

	peer.timersStart()

	device.flushInboundQueue(peer.queue.inbound)
	device.flushOutboundQueue(peer.queue.outbound)

	// Use the device batch size, not the bind batch size, as the device size is
	// the size of the batch pools.
	batchSize := peer.device.BatchSize()
	go peer.RoutineSequentialSender(batchSize)
	go peer.RoutineSequentialReceiver(batchSize)

	peer.isRunning.Store(true)

	// Register callbacks with bond manager
	if device.bondMgr != nil {
		peerRef := peer

		// ARQ send callback — injects packets into WireGuard send path
		device.bondMgr.SetPeerSendFunc(peer.bondPeerID, func(data []byte) {
			if !peerRef.isRunning.Load() {
				return
			}
			elem := device.NewOutboundElement()
			copy(elem.buffer[MessageTransportHeaderSize:], data)
			elem.packet = elem.buffer[MessageTransportHeaderSize : MessageTransportHeaderSize+len(data)]
			container := device.GetOutboundElementsContainer()
			container.elems = append(container.elems, elem)
			peerRef.StagePackets(container)
			peerRef.SendStagedPackets()
		})

		// Jitter buffer TUN writer — delivers packets from playout goroutine.
		// Must zero the virtio header space (bytes before MessageTransportOffsetContent)
		// because the TUN driver with IFF_VNET_HDR expects a valid virtio_net_hdr.
		device.bondMgr.SetTUNWriter(peer.bondPeerID, func(data []byte) {
			if device.isClosed() {
				return
			}
			buf := device.GetMessageBuffer()
			// Zero the header area (virtio_net_hdr + any padding)
			for i := 0; i < MessageTransportOffsetContent; i++ {
				buf[i] = 0
			}
			copy(buf[MessageTransportOffsetContent:], data)
			bufs := [][]byte{buf[:MessageTransportOffsetContent+len(data)]}
			_, err := device.tun.device.Write(bufs, MessageTransportOffsetContent)
			if err != nil {
				device.log.Errorf("Jitter buffer TUN write failed: %v", err)
			}
			device.PutMessageBuffer(buf)
		})
	}
}

func (peer *Peer) ZeroAndFlushAll() {
	device := peer.device

	// clear key pairs

	keypairs := &peer.keypairs
	keypairs.Lock()
	device.DeleteKeypair(keypairs.previous)
	device.DeleteKeypair(keypairs.current)
	device.DeleteKeypair(keypairs.next.Load())
	keypairs.previous = nil
	keypairs.current = nil
	keypairs.next.Store(nil)
	keypairs.Unlock()

	// clear handshake state

	handshake := &peer.handshake
	handshake.mutex.Lock()
	device.indexTable.Delete(handshake.localIndex)
	handshake.Clear()
	handshake.mutex.Unlock()

	peer.FlushStagedPackets()
}

func (peer *Peer) ExpireCurrentKeypairs() {
	handshake := &peer.handshake
	handshake.mutex.Lock()
	peer.device.indexTable.Delete(handshake.localIndex)
	handshake.Clear()
	peer.handshake.lastSentHandshake = time.Now().Add(-(RekeyTimeout + time.Second))
	handshake.mutex.Unlock()

	keypairs := &peer.keypairs
	keypairs.Lock()
	if keypairs.current != nil {
		keypairs.current.sendNonce.Store(RejectAfterMessages)
	}
	if next := keypairs.next.Load(); next != nil {
		next.sendNonce.Store(RejectAfterMessages)
	}
	keypairs.Unlock()
}

func (peer *Peer) Stop() {
	peer.state.Lock()
	defer peer.state.Unlock()

	if !peer.isRunning.Swap(false) {
		return
	}

	peer.device.log.Verbosef("%v - Stopping", peer)

	peer.timersStop()
	// Signal that RoutineSequentialSender and RoutineSequentialReceiver should exit.
	peer.queue.inbound.c <- nil
	peer.queue.outbound.c <- nil
	peer.stopping.Wait()
	peer.device.queue.encryption.wg.Done() // no more writes to encryption queue from us

	peer.ZeroAndFlushAll()

	// Close bond path sockets
	peer.ClearBondPaths()
}

type discoveredEndpoint struct {
	endpoint conn.Endpoint
	lastSeen time.Time
}

// maxEndpointsPerIP limits how many distinct port mappings we track per
// unique source IP. A client with N bond paths behind the same NAT will
// produce N different source ports on the same public IP — we keep the
// N most recently seen. When NAT ports rotate, old entries are replaced
// rather than accumulated.
const maxEndpointsPerIP = 3

// maxEndpointsPerPeer is the global cap on discovered endpoints per peer.
// Prevents unbounded growth when a client roams across many networks.
// When exceeded, the globally oldest endpoint is evicted.
const maxEndpointsPerPeer = 8

// endpointGCTimeout is a long garbage-collection timeout for endpoints
// that are truly gone (e.g. client physically disconnected an interface).
// This is NOT a liveness check — it is deliberately much longer than
// any keepalive or probe interval to avoid death spirals where eviction
// removes an endpoint, stopping return traffic, which prevents the
// endpoint from ever refreshing.
//
// Design rationale:
//   - WireGuard persistent-keepalive: 25s
//   - Bond probes: 1s (but only traverse active paths)
//   - RFC 4787 minimum UDP NAT mapping: 2 minutes
//   - Tailscale direct-path expiry: 2 minutes
//   - MPTCP subflow timeout: 15-60s (but MPTCP never evicts, only marks failed)
//
// 2 minutes (120s) matches RFC 4787 minimum UDP NAT mapping timeout.
// With stable sockets (idempotent AddBondPath, lock file script), port
// rotation no longer causes death spirals. Per-IP and per-peer caps
// handle NAT rotation and interface roaming.
const endpointGCTimeout = 2 * time.Minute

func (peer *Peer) SetEndpointFromPacket(endpoint conn.Endpoint) {
	peer.endpoint.Lock()
	defer peer.endpoint.Unlock()
	if peer.endpoint.disableRoaming {
		return
	}
	peer.endpoint.clearSrcOnTx = false
	peer.endpoint.val = endpoint

	// Track this source address for multi-path send.
	// Key by IP:port so each NAT mapping is a distinct entry, but
	// cap the number of entries per unique IP to maxEndpointsPerIP.
	// This prevents accumulation when NAT ports rotate while still
	// supporting multiple bond paths behind the same NAT.
	key := endpoint.DstToString()
	ipKey := endpoint.DstIP().String()
	now := time.Now()

	if peer.endpoint.discovered == nil {
		peer.endpoint.discovered = make(map[string]discoveredEndpoint)
	}
	peer.endpoint.discovered[key] = discoveredEndpoint{
		endpoint: endpoint,
		lastSeen: now,
	}

	// Garbage-collect endpoints not seen in a very long time (10 min).
	// This is a safety net, not a liveness check. Short timeouts cause
	// death spirals: eviction stops send traffic to that endpoint, which
	// prevents return traffic, so the endpoint never refreshes.
	// The per-IP and per-peer caps below handle the important cases
	// (NAT port rotation, interface roaming) without this risk.
	for k, de := range peer.endpoint.discovered {
		if now.Sub(de.lastSeen) > endpointGCTimeout {
			delete(peer.endpoint.discovered, k)
		}
	}

	// Cap endpoints per IP: if more than maxEndpointsPerIP entries share
	// the same source IP, evict the oldest until we're at the limit.
	for {
		var sameIPKeys []string
		for k, de := range peer.endpoint.discovered {
			if de.endpoint.DstIP().String() == ipKey {
				sameIPKeys = append(sameIPKeys, k)
			}
		}
		if len(sameIPKeys) <= maxEndpointsPerIP {
			break
		}
		// Find the oldest entry for this IP and remove it
		oldestKey := sameIPKeys[0]
		oldestTime := peer.endpoint.discovered[oldestKey].lastSeen
		for _, k := range sameIPKeys[1:] {
			if peer.endpoint.discovered[k].lastSeen.Before(oldestTime) {
				oldestKey = k
				oldestTime = peer.endpoint.discovered[k].lastSeen
			}
		}
		delete(peer.endpoint.discovered, oldestKey)
	}

	// Cap total endpoints per peer: prevents unbounded growth when
	// client roams across many different networks over time.
	for len(peer.endpoint.discovered) > maxEndpointsPerPeer {
		oldestKey := ""
		var oldestTime time.Time
		for k, de := range peer.endpoint.discovered {
			if oldestKey == "" || de.lastSeen.Before(oldestTime) {
				oldestKey = k
				oldestTime = de.lastSeen
			}
		}
		delete(peer.endpoint.discovered, oldestKey)
	}
}

func (peer *Peer) markEndpointSrcForClearing() {
	peer.endpoint.Lock()
	defer peer.endpoint.Unlock()
	if peer.endpoint.val == nil {
		return
	}
	peer.endpoint.clearSrcOnTx = true
}

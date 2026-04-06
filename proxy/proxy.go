/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

// Package proxy implements a UDP proxy that sits between kernel WireGuard
// and the network, applying FEC, reorder, and ARQ on encrypted WireGuard
// transport messages.
//
// Architecture:
//
//	App → wg0 (kernel WireGuard, encryption) → loopback → 007 proxy → multi-path send
//	Remote: multi-path recv → 007 proxy → loopback → wg0 (kernel WireGuard, decryption) → App
//
// The proxy intercepts WireGuard's encrypted UDP packets on loopback and
// applies bond processing (FEC/reorder/ARQ) on the ciphertext before
// sending out all configured network paths. FEC operates identically on
// ciphertext — Reed-Solomon encodes bytes regardless of content.
package proxy

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/bond"
)

// Config holds proxy configuration.
type Config struct {
	// WireGuard-facing (loopback)
	WGListenAddr  string // address to listen for packets from kernel wg0 (e.g., "127.0.0.1:51821")
	WGForwardAddr string // address to forward recovered packets to wg0 (e.g., "127.0.0.1:51820")

	// Network-facing
	ListenPort int    // port to listen on all paths for remote proxy packets (e.g., 51822)
	RemoteAddr string // remote 007 proxy address (optional — learned from first inbound if empty)

	// Network paths
	Paths []PathConfig // one per physical interface

	// Bond
	BondConfig bond.Config

	// API
	APIAddr string
	APIKey  string

	// Logger
	Logger bond.Logger
}

// PathConfig describes a network path bound to a specific interface.
type PathConfig struct {
	Name    string // interface name (for logging)
	LocalIP string // local IP to bind (e.g., "192.168.1.100")
}

// Proxy is the core UDP proxy between kernel WireGuard and the network.
type Proxy struct {
	config Config
	bond   *bond.Manager
	api    *bond.API

	// WireGuard-facing (loopback)
	wgConn *net.UDPConn   // listens for packets from kernel wg0
	wgDst  *net.UDPAddr   // forward recovered inbound to wg0's listen port
	wgSrc  *net.UDPAddr   // our loopback address (so wg0 sees correct source)

	// Network-facing (multi-path)
	paths      []*pathSocket
	remote     *net.UDPAddr  // remote 007 proxy endpoint (may be nil until learned)
	remoteMu   sync.RWMutex // guards remote

	// Lifecycle
	mu       sync.Mutex
	running  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
	peerID   uint32
	logger   bond.Logger
}

type pathSocket struct {
	id      int
	name    string
	conn    *net.UDPConn
	localIP netip.Addr
}

// New creates a new proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Logger == nil {
		cfg.Logger = bond.NewStdLogger(log.New(log.Writer(), "[007-proxy] ", log.LstdFlags))
	}

	// Parse addresses
	wgListenAddr, err := net.ResolveUDPAddr("udp", cfg.WGListenAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid wg-listen address: %w", err)
	}

	wgForwardAddr, err := net.ResolveUDPAddr("udp", cfg.WGForwardAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid wg-forward address: %w", err)
	}

	var remoteAddr *net.UDPAddr
	if cfg.RemoteAddr != "" {
		remoteAddr, err = net.ResolveUDPAddr("udp", cfg.RemoteAddr)
		if err != nil {
			return nil, fmt.Errorf("invalid remote address: %w", err)
		}
	}

	// Create bond manager
	bondMgr, err := bond.NewManager(cfg.BondConfig, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create bond manager: %w", err)
	}

	p := &Proxy{
		config: cfg,
		bond:   bondMgr,
		wgDst:  wgForwardAddr,
		wgSrc:  wgListenAddr,
		remote: remoteAddr,
		stopCh: make(chan struct{}),
		peerID: 1,
		logger: cfg.Logger,
	}

	// Listen on loopback for packets from kernel wg0
	p.wgConn, err = net.ListenUDP("udp", wgListenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", cfg.WGListenAddr, err)
	}

	// Create per-path sockets bound to specific local IPs
	for i, pc := range cfg.Paths {
		localIP, err := netip.ParseAddr(pc.LocalIP)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("invalid path local IP %s: %w", pc.LocalIP, err)
		}

		var network string
		if localIP.Is4() {
			network = "udp4"
		} else {
			network = "udp6"
		}

		localAddr := &net.UDPAddr{IP: localIP.AsSlice(), Port: cfg.ListenPort}
		conn, err := net.ListenUDP(network, localAddr)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("failed to bind path %s to %s: %w", pc.Name, pc.LocalIP, err)
		}

		p.paths = append(p.paths, &pathSocket{
			id:      i,
			name:    pc.Name,
			conn:    conn,
			localIP: localIP,
		})
	}

	// If no paths configured, create a default unbound socket
	if len(p.paths) == 0 {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.ListenPort})
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("failed to create default path: %w", err)
		}
		p.paths = append(p.paths, &pathSocket{
			id:   0,
			name: "default",
			conn: conn,
		})
	}

	return p, nil
}

// Start begins the proxy goroutines.
func (p *Proxy) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return
	}
	p.running = true

	// Start bond manager
	p.bond.Start()

	// Register send function for ARQ/control packets
	p.bond.SetPeerSendFunc(p.peerID, func(data []byte) {
		p.sendAllPaths(data)
	})

	// Start API
	if p.config.APIAddr != "" {
		p.api = bond.NewAPI(p.bond, p.config.APIAddr, p.config.APIKey)
		p.api.Start()
	}

	// Outbound: kernel wg0 → bond encode → multi-path send
	p.wg.Add(1)
	go p.outboundLoop()

	// Inbound: multi-path recv → bond decode → kernel wg0
	for _, path := range p.paths {
		p.wg.Add(1)
		go p.inboundLoop(path)
	}

	p.logger.Info("proxy started",
		"wg_listen", p.config.WGListenAddr,
		"wg_forward", p.config.WGForwardAddr,
		"remote", p.config.RemoteAddr,
		"paths", len(p.paths))
}

// Stop shuts down the proxy.
func (p *Proxy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return
	}
	p.running = false

	close(p.stopCh)
	p.wgConn.Close()
	for _, path := range p.paths {
		path.conn.Close()
	}
	p.wg.Wait()
	p.bond.Stop()
	if p.api != nil {
		p.api.Stop()
	}
	p.logger.Info("proxy stopped")
}

// Close releases all resources.
func (p *Proxy) Close() {
	p.Stop()
	if p.wgConn != nil {
		p.wgConn.Close()
	}
	for _, path := range p.paths {
		path.conn.Close()
	}
}

// outboundLoop reads encrypted packets from kernel wg0, applies FEC, sends on all paths.
func (p *Proxy) outboundLoop() {
	defer p.wg.Done()

	buf := make([]byte, 65536)
	for {
		n, _, err := p.wgConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-p.stopCh:
				return
			default:
				p.logger.Error("wg read error", "err", err)
				time.Sleep(time.Millisecond)
				continue
			}
		}

		// Make a copy for bond processing
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		// Bond encode: prepends FEC header, generates parity when block fills
		encoded := p.bond.ProcessOutbound(p.peerID, pkt, 0)

		// Send each encoded packet (data + parity) on all paths
		for _, epkt := range encoded {
			p.sendAllPaths(epkt)
		}
	}
}

// inboundLoop reads packets from one network path, applies bond decode, forwards to wg0.
func (p *Proxy) inboundLoop(path *pathSocket) {
	defer p.wg.Done()

	buf := make([]byte, 65536)
	for {
		n, srcAddr, err := path.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-p.stopCh:
				return
			default:
				p.logger.Error("path read error", "path", path.name, "err", err)
				time.Sleep(time.Millisecond)
				continue
			}
		}

		// Learn remote address from first inbound packet (ignore loopback/self)
		if srcAddr != nil {
			srcIP := srcAddr.IP
			isLocal := srcIP.IsLoopback()
			if !isLocal {
				// Check if source is one of our own path IPs
				for _, pp := range p.paths {
					if pp.localIP.IsValid() && srcIP.Equal(pp.localIP.AsSlice()) {
						isLocal = true
						break
					}
				}
			}
			if !isLocal {
				p.remoteMu.RLock()
				known := p.remote != nil
				p.remoteMu.RUnlock()
				if !known {
					p.remoteMu.Lock()
					if p.remote == nil {
						p.remote = srcAddr
						p.logger.Info("learned remote address", "addr", srcAddr.String(), "path", path.name)
					}
					p.remoteMu.Unlock()
				}
			}
		}

		// Make a copy for bond processing
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		// Bond decode: FEC recovery, reorder, ARQ
		ready := p.bond.ProcessInbound(p.peerID, pkt, 0, path.id)

		// Forward recovered WireGuard packets to kernel wg0
		for _, rpkt := range ready {
			_, err := p.wgConn.WriteToUDP(rpkt, p.wgDst)
			if err != nil {
				p.logger.Error("wg forward error", "err", err)
			}
		}
	}
}

// sendAllPaths sends a packet on all configured network paths.
func (p *Proxy) sendAllPaths(pkt []byte) {
	p.remoteMu.RLock()
	dst := p.remote
	p.remoteMu.RUnlock()
	if dst == nil {
		return // don't know where to send yet
	}
	for _, path := range p.paths {
		path.conn.WriteToUDP(pkt, dst)
	}
}

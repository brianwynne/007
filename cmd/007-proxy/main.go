/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.zx2c4.com/wireguard/bond"
	"golang.zx2c4.com/wireguard/proxy"
)

func main() {
	var (
		wgListen   = flag.String("wg-listen", "127.0.0.1:51821", "Listen for packets from kernel wg0")
		wgForward  = flag.String("wg-forward", "127.0.0.1:51820", "Forward recovered packets to wg0")
		listenPort = flag.Int("listen-port", 51822, "Network port to listen for remote proxy packets")
		remote     = flag.String("remote", "", "Remote 007 proxy address (optional — learned from first inbound)")
		pathFlags  pathList
		apiAddr    = flag.String("api", "127.0.0.1:8007", "Management API listen address")
		apiKey     = flag.String("api-key", "", "API key (optional)")
		version    = flag.Bool("version", false, "Print version and exit")
	)
	flag.Var(&pathFlags, "path", "Network path as name=localip (repeatable, e.g., -path eth0=192.168.1.100)")
	flag.Parse()

	if *version {
		fmt.Println("007 Bond Proxy v0.2.0")
		fmt.Println("Multi-path bonding proxy for kernel WireGuard")
		fmt.Println("https://github.com/brianwynne/007")
		return
	}

	logger := bond.NewStdLogger(log.New(os.Stderr, "[007] ", log.LstdFlags))

	// Parse path configs
	var paths []proxy.PathConfig
	for _, p := range pathFlags {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Error: invalid path format %q (use name=localip)\n", p)
			os.Exit(1)
		}
		paths = append(paths, proxy.PathConfig{
			Name:    parts[0],
			LocalIP: parts[1],
		})
	}

	// Bond config from environment
	bondCfg := bond.DefaultConfig()
	if os.Getenv("BOND_FEC") == "0" {
		bondCfg.FECEnabled = false
	}
	if os.Getenv("BOND_REORDER") == "0" {
		bondCfg.ReorderEnabled = false
	}

	cfg := proxy.Config{
		WGListenAddr:  *wgListen,
		WGForwardAddr: *wgForward,
		ListenPort:    *listenPort,
		RemoteAddr:    *remote,
		Paths:         paths,
		BondConfig:    bondCfg,
		APIAddr:       *apiAddr,
		APIKey:        *apiKey,
		Logger:        logger,
	}

	p, err := proxy.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	p.Start()

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	<-sig

	p.Stop()
}

// pathList implements flag.Value for repeatable -path flags.
type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ", ") }
func (p *pathList) Set(v string) error {
	*p = append(*p, v)
	return nil
}

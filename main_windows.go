/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"

	"golang.org/x/sys/windows"

	"golang.zx2c4.com/wireguard/bond"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/ipc"
	"golang.zx2c4.com/wireguard/tun"
)

const (
	ExitSetupSuccess = 0
	ExitSetupFailed  = 1
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("007 Bond v%s\n\nMulti-path network bonding for windows-%s.\nBased on wireguard-go. https://github.com/brianwynne/007\n", Version, "amd64")
		return
	}

	if len(os.Args) != 2 {
		fmt.Printf("Usage: %s INTERFACE-NAME\n", os.Args[0])
		fmt.Printf("\n007 Bond — Multi-path network bonding with FEC and reordering\n")
		os.Exit(ExitSetupFailed)
	}
	interfaceName := os.Args[1]

	logger := device.NewLogger(
		device.LogLevelVerbose,
		fmt.Sprintf("(%s) ", interfaceName),
	)
	logger.Verbosef("Starting 007 Bond version %s", Version)

	tunDev, err := tun.CreateTUN(interfaceName, 0)
	if err == nil {
		realInterfaceName, err2 := tunDev.Name()
		if err2 == nil {
			interfaceName = realInterfaceName
		}
	} else {
		logger.Errorf("Failed to create TUN device: %v", err)
		os.Exit(ExitSetupFailed)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), logger)

	// 007 Bond: Create and attach bond manager
	bondCfg := bond.DefaultConfig()
	if os.Getenv("BOND_FEC") == "0" {
		bondCfg.FECEnabled = false
	}
	if os.Getenv("BOND_REORDER") == "0" {
		bondCfg.ReorderEnabled = false
	}
	bondLogger := bond.NewStdLogger(log.New(os.Stderr, fmt.Sprintf("(%s) ", interfaceName), log.LstdFlags))
	bondMgr, err := bond.NewManager(bondCfg, bondLogger)
	if err != nil {
		logger.Errorf("Failed to create bond manager: %v", err)
		os.Exit(ExitSetupFailed)
	}
	dev.SetBondManager(bondMgr)
	bondMgr.Start()

	// Management API
	apiAddr := os.Getenv("BOND_API")
	if apiAddr == "" {
		apiAddr = "127.0.0.1:8007"
	}
	apiKey := os.Getenv("BOND_API_KEY")
	bondAPI := bond.NewAPI(bondMgr, apiAddr, apiKey)
	bondAPI.Start()

	logger.Verbosef("007 Bond started (FEC=%v, Reorder=%v, API=%s)", bondCfg.FECEnabled, bondCfg.ReorderEnabled, apiAddr)

	err = dev.Up()
	if err != nil {
		logger.Errorf("Failed to bring up device: %v", err)
		os.Exit(ExitSetupFailed)
	}
	logger.Verbosef("Device started")

	uapi, err := ipc.UAPIListen(interfaceName)
	if err != nil {
		logger.Errorf("Failed to listen on uapi socket: %v", err)
		os.Exit(ExitSetupFailed)
	}

	errs := make(chan error)
	term := make(chan os.Signal, 1)

	go func() {
		for {
			conn, err := uapi.Accept()
			if err != nil {
				errs <- err
				return
			}
			go dev.IpcHandle(conn)
		}
	}()
	logger.Verbosef("UAPI listener started")

	signal.Notify(term, os.Interrupt)
	signal.Notify(term, os.Kill)
	signal.Notify(term, windows.SIGTERM)

	select {
	case <-term:
	case <-errs:
	case <-dev.Wait():
	}

	bondAPI.Stop()
	bondMgr.Stop()
	uapi.Close()
	dev.Close()

	logger.Verbosef("Shutting down")
}

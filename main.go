//go:build !windows

/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
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

const (
	ENV_WG_TUN_FD             = "WG_TUN_FD"
	ENV_WG_UAPI_FD            = "WG_UAPI_FD"
	ENV_WG_PROCESS_FOREGROUND = "WG_PROCESS_FOREGROUND"
)

func printUsage() {
	fmt.Printf("Usage: %s [-f/--foreground] INTERFACE-NAME\n\n", os.Args[0])
	fmt.Printf("007 Bond — Multi-path network bonding with FEC, ARQ, and jitter buffer\n\n")
	fmt.Printf("Options:\n")
	fmt.Printf("  -f, --foreground    Run in foreground (default: daemonize)\n")
	fmt.Printf("  --version           Show version and exit\n")
	fmt.Printf("  --help              Show this help\n\n")
	fmt.Printf("Environment Variables:\n")
	fmt.Printf("  BOND_PRESET         Latency preset (default: field)\n")
	fmt.Printf("                        broadcast  40ms  — live broadcast, 20ms jitter buffer\n")
	fmt.Printf("                        studio     80ms  — studio links, 60ms jitter buffer\n")
	fmt.Printf("                        field      200ms — WiFi + cellular, 180ms jitter buffer\n")
	fmt.Printf("  BOND_FEC_MODE       FEC strategy: block (default) or sliding (XOR window)\n")
	fmt.Printf("  BOND_FEC            Set to 0 to disable FEC\n")
	fmt.Printf("  BOND_JITTER         Set to 0 to disable jitter buffer (use legacy reorder)\n")
	fmt.Printf("  BOND_REORDER        Set to 0 to disable reorder buffer\n")
	fmt.Printf("  BOND_API            Management API listen address (default: 127.0.0.1:8007)\n")
	fmt.Printf("  BOND_API_KEY        Optional API authentication key\n")
	fmt.Printf("  LOG_LEVEL           Logging: verbose, error (default), silent\n\n")
	fmt.Printf("Bond paths are configured via WireGuard UAPI:\n")
	fmt.Printf("  wg set bond0 peer <pubkey> bond_endpoint=<server>:51820@<local_ip>\n\n")
	fmt.Printf("Management API:\n")
	fmt.Printf("  GET /api/stats      FEC, ARQ, jitter buffer statistics\n")
	fmt.Printf("  GET /api/paths      Per-path health (RTT, loss, jitter)\n")
	fmt.Printf("  GET /api/config     Current configuration\n")
	fmt.Printf("  GET /api/health     Health check\n")
	fmt.Printf("  POST /api/reload    Reload config from /etc/007/.env\n")
}

// loadEnvFile reads a KEY=VALUE environment file and returns the values as a map.
func loadEnvFile(path string) map[string]string {
	env := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return env
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}
	return env
}

// loadBondConfig builds a bond.Config from environment variables.
func loadBondConfig() bond.Config {
	var cfg bond.Config
	switch os.Getenv("BOND_PRESET") {
	case "broadcast":
		cfg = bond.BroadcastPreset()
	case "studio":
		cfg = bond.StudioPreset()
	case "field":
		cfg = bond.FieldPreset()
	default:
		cfg = bond.DefaultConfig()
	}
	if os.Getenv("BOND_FEC") == "0" {
		cfg.FECEnabled = false
	}
	if os.Getenv("BOND_FEC_MODE") == "sliding" {
		cfg.FECMode = "sliding"
	}
	if os.Getenv("BOND_JITTER") == "0" {
		cfg.JitterEnabled = false
		cfg.ReorderEnabled = true
		cfg.ReorderBufSize = 64
		cfg.ReorderWindowMs = 80
		cfg.ReorderMinMs = 20
		cfg.ReorderMaxMs = 200
		cfg.ReorderFlushMs = 10
		cfg.ReorderAdaptSec = 1
	}
	if os.Getenv("BOND_REORDER") == "0" {
		cfg.ReorderEnabled = false
	}
	return cfg
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("007 Bond v%s (%s-%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return
	}

	if len(os.Args) == 2 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
		printUsage()
		return
	}

	var foreground bool
	var interfaceName string
	if len(os.Args) < 2 || len(os.Args) > 3 {
		printUsage()
		return
	}

	switch os.Args[1] {

	case "-f", "--foreground":
		foreground = true
		if len(os.Args) != 3 {
			printUsage()
			return
		}
		interfaceName = os.Args[2]

	default:
		foreground = false
		if len(os.Args) != 2 {
			printUsage()
			return
		}
		interfaceName = os.Args[1]
	}

	if !foreground {
		foreground = os.Getenv(ENV_WG_PROCESS_FOREGROUND) == "1"
	}

	// get log level (default: info)

	logLevel := func() int {
		switch os.Getenv("LOG_LEVEL") {
		case "verbose", "debug":
			return device.LogLevelVerbose
		case "error":
			return device.LogLevelError
		case "silent":
			return device.LogLevelSilent
		}
		return device.LogLevelError
	}()

	// open TUN device (or use supplied fd)

	tdev, err := func() (tun.Device, error) {
		tunFdStr := os.Getenv(ENV_WG_TUN_FD)
		if tunFdStr == "" {
			return tun.CreateTUN(interfaceName, device.DefaultMTU)
		}

		// construct tun device from supplied fd

		fd, err := strconv.ParseUint(tunFdStr, 10, 32)
		if err != nil {
			return nil, err
		}

		err = unix.SetNonblock(int(fd), true)
		if err != nil {
			return nil, err
		}

		file := os.NewFile(uintptr(fd), "")
		return tun.CreateTUNFromFile(file, device.DefaultMTU)
	}()

	if err == nil {
		realInterfaceName, err2 := tdev.Name()
		if err2 == nil {
			interfaceName = realInterfaceName
		}
	}

	logger := device.NewLogger(
		logLevel,
		fmt.Sprintf("(%s) ", interfaceName),
	)

	logger.Verbosef("Starting wireguard-go version %s", Version)

	if err != nil {
		logger.Errorf("Failed to create TUN device: %v", err)
		os.Exit(ExitSetupFailed)
	}

	// open UAPI file (or use supplied fd)

	fileUAPI, err := func() (*os.File, error) {
		uapiFdStr := os.Getenv(ENV_WG_UAPI_FD)
		if uapiFdStr == "" {
			return ipc.UAPIOpen(interfaceName)
		}

		// use supplied fd

		fd, err := strconv.ParseUint(uapiFdStr, 10, 32)
		if err != nil {
			return nil, err
		}

		return os.NewFile(uintptr(fd), ""), nil
	}()
	if err != nil {
		logger.Errorf("UAPI listen error: %v", err)
		os.Exit(ExitSetupFailed)
		return
	}
	// daemonize the process

	if !foreground {
		env := os.Environ()
		env = append(env, fmt.Sprintf("%s=3", ENV_WG_TUN_FD))
		env = append(env, fmt.Sprintf("%s=4", ENV_WG_UAPI_FD))
		env = append(env, fmt.Sprintf("%s=1", ENV_WG_PROCESS_FOREGROUND))
		files := [3]*os.File{}
		if os.Getenv("LOG_LEVEL") != "" && logLevel != device.LogLevelSilent {
			files[0], _ = os.Open(os.DevNull)
			files[1] = os.Stdout
			files[2] = os.Stderr
		} else {
			files[0], _ = os.Open(os.DevNull)
			files[1], _ = os.Open(os.DevNull)
			files[2], _ = os.Open(os.DevNull)
		}
		attr := &os.ProcAttr{
			Files: []*os.File{
				files[0], // stdin
				files[1], // stdout
				files[2], // stderr
				tdev.File(),
				fileUAPI,
			},
			Dir: ".",
			Env: env,
		}

		path, err := os.Executable()
		if err != nil {
			logger.Errorf("Failed to determine executable: %v", err)
			os.Exit(ExitSetupFailed)
		}

		process, err := os.StartProcess(
			path,
			os.Args,
			attr,
		)
		if err != nil {
			logger.Errorf("Failed to daemonize: %v", err)
			os.Exit(ExitSetupFailed)
		}
		process.Release()
		return
	}

	dev := device.NewDevice(tdev, conn.NewDefaultBind(), logger)

	// 007 Bond: Create and attach bond manager
	bondCfg := loadBondConfig()
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

	errs := make(chan error)
	term := make(chan os.Signal, 1)

	uapi, err := ipc.UAPIListen(interfaceName, fileUAPI)
	if err != nil {
		logger.Errorf("Failed to listen on uapi socket: %v", err)
		os.Exit(ExitSetupFailed)
	}

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

	// SIGHUP: reload config without restart
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, unix.SIGHUP)
	go func() {
		for range reload {
			logger.Verbosef("SIGHUP received, reloading config...")
			// Re-read env file into process environment
			envFile := "/etc/007/.env"
			for k, v := range loadEnvFile(envFile) {
				os.Setenv(k, v)
			}
			// Apply new preset
			preset := os.Getenv("BOND_PRESET")
			if preset != "" {
				if err := bondMgr.SetPreset(preset); err != nil {
					logger.Errorf("Reload preset failed: %v", err)
				} else {
					logger.Verbosef("Reloaded preset: %s", preset)
				}
			}
			logger.Verbosef("Config reload complete")
		}
	}()

	// wait for program to terminate

	signal.Notify(term, unix.SIGTERM)
	signal.Notify(term, os.Interrupt)

	select {
	case <-term:
	case <-errs:
	case <-dev.Wait():
	}

	// clean up

	bondAPI.Stop()
	bondMgr.Stop()
	uapi.Close()
	dev.Close()

	logger.Verbosef("Shutting down")
}

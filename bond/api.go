/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

package bond

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// API provides a REST management interface for monitoring and controlling
// the bond manager. Exposes stats, path health, and configuration.
//
// Endpoints:
//   GET /api/stats   — bond statistics (FEC, reorder, ARQ, paths)
//   GET /api/paths   — per-path health (RTT, loss, jitter)
//   GET /api/config  — current configuration
//   GET /api/health  — simple health check
type API struct {
	mgr    *Manager
	server *http.Server
}

// NewAPI creates a management API server.
func NewAPI(mgr *Manager, listenAddr string) *API {
	a := &API{mgr: mgr}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", a.handleStats)
	mux.HandleFunc("/api/paths", a.handlePaths)
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/health", a.handleHealth)

	a.server = &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	return a
}

// Start begins serving the API in a background goroutine.
func (a *API) Start() {
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			if a.mgr.logger != nil {
				a.mgr.logger.Printf("007 Bond: API server error: %v", err)
			}
		}
	}()
}

// Stop shuts down the API server.
func (a *API) Stop() {
	a.server.Close()
}

// statsResponse is the JSON response for /api/stats.
type statsResponse struct {
	Uptime           string             `json:"uptime"`
	TxPackets        uint64             `json:"tx_packets"`
	RxPackets        uint64             `json:"rx_packets"`
	FECRecovered     uint64             `json:"fec_recovered"`
	FECFailed        uint64             `json:"fec_failed"`
	ReorderInOrder   uint64             `json:"reorder_in_order"`
	ReorderReordered uint64             `json:"reorder_reordered"`
	ReorderGaps      uint64             `json:"reorder_gaps"`
	ReorderWindowMs  int64              `json:"reorder_window_ms"`
	Paths            []pathResponse     `json:"paths"`
}

type pathResponse struct {
	PathID  int     `json:"path_id"`
	RTTMs   float64 `json:"rtt_ms"`
	JitterMs float64 `json:"jitter_ms"`
	Loss    float64 `json:"loss"`
	RxCount uint64  `json:"rx_count"`
}

func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s := a.mgr.GetStats()

	resp := statsResponse{
		TxPackets:        s.TxPackets,
		RxPackets:        s.RxPackets,
		FECRecovered:     s.FECRecovered,
		FECFailed:        s.FECFailed,
		ReorderInOrder:   s.ReorderInOrder,
		ReorderReordered: s.ReorderReordered,
		ReorderGaps:      s.ReorderGaps,
		ReorderWindowMs:  s.ReorderWindowMs,
	}

	for _, p := range s.Paths {
		resp.Paths = append(resp.Paths, pathResponse{
			PathID:   p.PathID,
			RTTMs:    float64(p.RTT) / float64(time.Millisecond),
			JitterMs: float64(p.Jitter) / float64(time.Millisecond),
			Loss:     p.Loss,
			RxCount:  p.RxCount,
		})
	}

	writeJSON(w, resp)
}

func (a *API) handlePaths(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s := a.mgr.GetStats()

	var paths []pathResponse
	for _, p := range s.Paths {
		paths = append(paths, pathResponse{
			PathID:   p.PathID,
			RTTMs:    float64(p.RTT) / float64(time.Millisecond),
			JitterMs: float64(p.Jitter) / float64(time.Millisecond),
			Loss:     p.Loss,
			RxCount:  p.RxCount,
		})
	}

	writeJSON(w, paths)
}

type configResponse struct {
	FECEnabled     bool `json:"fec_enabled"`
	FECAdaptive    bool `json:"fec_adaptive"`
	ReorderEnabled bool `json:"reorder_enabled"`
}

func (a *API) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := configResponse{
		FECEnabled:     a.mgr.config.FECEnabled,
		FECAdaptive:    a.mgr.config.FECAdaptive,
		ReorderEnabled: a.mgr.config.ReorderEnabled,
	}

	writeJSON(w, resp)
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, fmt.Sprintf("json encode error: %v", err), http.StatusInternalServerError)
	}
}

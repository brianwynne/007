/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2026 007 Bond Project. All Rights Reserved.
 */

package bond

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	apiKey string // optional API key for authentication
}

// NewAPI creates a management API server.
// listenAddr defaults to 127.0.0.1:8007 if empty.
// apiKey is optional — if set, all requests must include X-API-Key header.
func NewAPI(mgr *Manager, listenAddr, apiKey string) *API {
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8007"
	}
	// Warn if binding to all interfaces
	if strings.HasPrefix(listenAddr, ":") || strings.HasPrefix(listenAddr, "0.0.0.0:") {
		mgr.logger.Warn("API server binding to all interfaces — consider restricting to localhost",
			"addr", listenAddr)
	}

	a := &API{mgr: mgr, apiKey: apiKey}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", a.auth(a.handleStats))
	mux.HandleFunc("/api/paths", a.auth(a.handlePaths))
	mux.HandleFunc("/api/config", a.auth(a.handleConfig))
	mux.HandleFunc("/api/health", a.handleHealth) // health check unauthenticated

	a.server = &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	return a
}

// auth wraps a handler with API key authentication (if configured).
func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.apiKey != "" {
			key := r.Header.Get("X-API-Key")
			if key != a.apiKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// Start begins serving the API in a background goroutine.
func (a *API) Start() {
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.mgr.logger.Error("API server error", "err", err)
		}
	}()
}

// Stop shuts down the API server.
func (a *API) Stop() {
	a.server.Close()
}

// statsResponse is the JSON response for /api/stats.
type statsResponse struct {
	SystemState       string             `json:"system_state"`
	TxPackets         uint64             `json:"tx_packets"`
	RxPackets         uint64             `json:"rx_packets"`
	DropPackets       uint64             `json:"drop_packets"`
	DuplicatePackets  uint64             `json:"duplicate_packets"`
	FECRecovered      uint64             `json:"fec_recovered"`
	FECFailed         uint64             `json:"fec_failed"`
	ReorderInOrder    uint64             `json:"reorder_in_order"`
	ReorderReordered  uint64             `json:"reorder_reordered"`
	ReorderGaps       uint64             `json:"reorder_gaps"`
	ReorderDuplicates uint64             `json:"reorder_duplicates"`
	ReorderLate       uint64             `json:"reorder_late"`
	ReorderWindowMs   int64              `json:"reorder_window_ms"`
	NACKsSent         uint64             `json:"nacks_sent"`
	NACKsReceived     uint64             `json:"nacks_received"`
	ARQRetransmitOK   uint64             `json:"arq_retransmit_ok"`
	ARQRetransmitMiss uint64             `json:"arq_retransmit_miss"`
	ARQReceived       uint64             `json:"arq_received"`
	ARQDeadlineSkip   uint64             `json:"arq_deadline_skip"`
	Paths             []pathResponse     `json:"paths"`
}

type pathResponse struct {
	PathID    int     `json:"path_id"`
	State     string  `json:"state"`
	RTTMs     float64 `json:"rtt_ms"`
	JitterMs  float64 `json:"jitter_ms"`
	Loss      float64 `json:"loss"`
	BurstLoss int     `json:"burst_loss"`
	MaxBurst  int     `json:"max_burst"`
	RxCount   uint64  `json:"rx_count"`
	DropCount uint64  `json:"drop_count"`
}

func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s := a.mgr.GetStats()

	resp := statsResponse{
		SystemState:       s.SystemState,
		TxPackets:         s.TxPackets,
		RxPackets:         s.RxPackets,
		DropPackets:       s.DropPackets,
		DuplicatePackets:  s.DuplicatePackets,
		FECRecovered:      s.FECRecovered,
		FECFailed:         s.FECFailed,
		ReorderInOrder:    s.ReorderInOrder,
		ReorderReordered:  s.ReorderReordered,
		ReorderGaps:       s.ReorderGaps,
		ReorderDuplicates: s.ReorderDuplicates,
		ReorderLate:       s.ReorderLate,
		ReorderWindowMs:   s.ReorderWindowMs,
		NACKsSent:         s.NACKsSent,
		NACKsReceived:     s.NACKsReceived,
		ARQRetransmitOK:   s.ARQRetransmitOK,
		ARQRetransmitMiss: s.ARQRetransmitMiss,
		ARQReceived:       s.ARQReceived,
		ARQDeadlineSkip:   s.ARQDeadlineSkip,
	}

	for _, p := range s.Paths {
		resp.Paths = append(resp.Paths, pathResponse{
			PathID:    p.PathID,
			State:     p.State.String(),
			RTTMs:     float64(p.RTT) / float64(time.Millisecond),
			JitterMs:  float64(p.Jitter) / float64(time.Millisecond),
			Loss:      p.Loss,
			BurstLoss: p.BurstLoss,
			MaxBurst:  p.MaxBurst,
			RxCount:   p.RxCount,
			DropCount: p.DropCount,
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
			PathID:    p.PathID,
			State:     p.State.String(),
			RTTMs:     float64(p.RTT) / float64(time.Millisecond),
			JitterMs:  float64(p.Jitter) / float64(time.Millisecond),
			Loss:      p.Loss,
			BurstLoss: p.BurstLoss,
			MaxBurst:  p.MaxBurst,
			RxCount:   p.RxCount,
			DropCount: p.DropCount,
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

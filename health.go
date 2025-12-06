package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// HealthStatus represents the health check response structure
type HealthStatus struct {
	Status                  string `json:"status"`
	WorkerID                string `json:"worker_id"`
	Uptime                  string `json:"uptime"`
	TotalProcessed          int64  `json:"total_processed"`
	HighPriorityProcessed   int64  `json:"high_priority_processed"`
	NormalPriorityProcessed int64  `json:"normal_priority_processed"`
	EmailsSent              int64  `json:"emails_sent"`
	PushSent                int64  `json:"push_sent"`
	TotalErrors             int64  `json:"total_errors"`
}

// startHealthCheckServer starts the HTTP server for health checks
func startHealthCheckServer(metrics *Metrics) {
	go func() {
		http.HandleFunc("/health", healthCheckHandler(metrics))
		http.HandleFunc("/healthz", healthCheckHandler(metrics)) // Alternative endpoint for k8s

		addr := ":" + healthCheckPort
		log.Printf("Health check server starting on %s\n", addr)

		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("Health check server error: %v\n", err)
		}
	}()
}

// healthCheckHandler returns an HTTP handler for health checks
func healthCheckHandler(metrics *Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uptime := time.Since(metrics.StartTime)

		totalErrors := metrics.EmailErrors.Load() +
			metrics.PushErrors.Load() +
			metrics.RateLimitErrors.Load() +
			metrics.DatabaseErrors.Load()

		health := HealthStatus{
			Status:                  "healthy",
			WorkerID:                workerId,
			Uptime:                  uptime.String(),
			TotalProcessed:          metrics.HighPriorityProcessed.Load() + metrics.NormalPriorityProcessed.Load(),
			HighPriorityProcessed:   metrics.HighPriorityProcessed.Load(),
			NormalPriorityProcessed: metrics.NormalPriorityProcessed.Load(),
			EmailsSent:              metrics.EmailsSent.Load(),
			PushSent:                metrics.PushSent.Load(),
			TotalErrors:             totalErrors,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(health)
	}
}

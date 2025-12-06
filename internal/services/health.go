package services

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"go-notifications-worker/internal/config"
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

// StartHealthCheckServer starts the HTTP server for health checks
func StartHealthCheckServer(metrics *Metrics) {
	go func() {
		http.HandleFunc("/health", HealthCheckHandler(metrics))
		http.HandleFunc("/healthz", HealthCheckHandler(metrics)) // Alternative endpoint for k8s

		addr := ":" + config.HealthCheckPort
		log.Printf("Health check server starting on %s\n", addr)

		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("Health check server error: %v\n", err)
		}
	}()
}

// HealthCheckHandler returns an HTTP handler for health checks
func HealthCheckHandler(metrics *Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uptime := time.Since(metrics.StartTime)

		totalErrors := metrics.EmailErrors.Load() +
			metrics.PushErrors.Load() +
			metrics.RateLimitErrors.Load() +
			metrics.DatabaseErrors.Load()

		health := HealthStatus{
			Status:                  "healthy",
			WorkerID:                config.WorkerId,
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

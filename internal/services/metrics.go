package services

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

// Metrics tracks worker performance statistics
type Metrics struct {
	// Notifications processed
	HighPriorityProcessed   atomic.Int64
	NormalPriorityProcessed atomic.Int64
	EmailsSent              atomic.Int64
	PushSent                atomic.Int64

	// Errors
	EmailErrors     atomic.Int64
	PushErrors      atomic.Int64
	RateLimitErrors atomic.Int64
	DatabaseErrors  atomic.Int64

	// Rate limiting
	RateLimitWaitsHigh   atomic.Int64
	RateLimitWaitsNormal atomic.Int64

	// Timing
	TotalProcessingTimeMs      atomic.Int64
	TotalEmailProcessingTimeMs atomic.Int64
	TotalPushProcessingTimeMs  atomic.Int64
	StartTime                  time.Time
}

// NewMetrics creates a new Metrics instance
func NewMetrics() *Metrics {
	return &Metrics{
		StartTime: time.Now(),
	}
}

// LogMetricsPeriodically logs metrics at regular intervals
func LogMetricsPeriodically(ctx context.Context, m *Metrics, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Log final metrics before shutdown
			LogMetrics(m)
			return
		case <-ticker.C:
			LogMetrics(m)
		}
	}
}

// LogMetrics outputs current metrics to the log
func LogMetrics(m *Metrics) {
	uptime := time.Since(m.StartTime)
	uptimeSeconds := uptime.Seconds()

	highProcessed := m.HighPriorityProcessed.Load()
	normalProcessed := m.NormalPriorityProcessed.Load()
	totalProcessed := highProcessed + normalProcessed

	emailsSent := m.EmailsSent.Load()
	pushSent := m.PushSent.Load()

	emailErrors := m.EmailErrors.Load()
	pushErrors := m.PushErrors.Load()
	rateLimitErrors := m.RateLimitErrors.Load()
	dbErrors := m.DatabaseErrors.Load()

	rateLimitWaitsHigh := m.RateLimitWaitsHigh.Load()
	rateLimitWaitsNormal := m.RateLimitWaitsNormal.Load()

	avgProcessingTime := float64(0)
	if totalProcessed > 0 {
		avgProcessingTime = float64(m.TotalProcessingTimeMs.Load()) / float64(totalProcessed)
	}

	avgEmailProcessingTime := float64(0)
	if emailsSent+emailErrors > 0 {
		avgEmailProcessingTime = float64(m.TotalEmailProcessingTimeMs.Load()) / float64(emailsSent+emailErrors)
	}

	avgPushProcessingTime := float64(0)
	if pushSent+pushErrors > 0 {
		avgPushProcessingTime = float64(m.TotalPushProcessingTimeMs.Load()) / float64(pushSent+pushErrors)
	}

	throughput := float64(0)
	if uptimeSeconds > 0 {
		throughput = float64(totalProcessed) / uptimeSeconds
	}

	log.Println("========== METRICS REPORT ==========")
	log.Printf("Uptime: %v", uptime.Round(time.Second))
	log.Printf("Total Processed: %d (%.2f/sec)", totalProcessed, throughput)
	log.Printf("  - High Priority: %d", highProcessed)
	log.Printf("  - Normal Priority: %d", normalProcessed)
	log.Printf("By Type:")
	log.Printf("  - Emails Sent: %d", emailsSent)
	log.Printf("  - Push Sent: %d", pushSent)
	log.Printf("Errors:")
	log.Printf("  - Email Errors: %d", emailErrors)
	log.Printf("  - Push Errors: %d", pushErrors)
	log.Printf("  - Rate Limit Errors: %d", rateLimitErrors)
	log.Printf("  - Database Errors: %d", dbErrors)
	log.Printf("Rate Limiting:")
	log.Printf("  - High Priority Waits: %d", rateLimitWaitsHigh)
	log.Printf("  - Normal Priority Waits: %d", rateLimitWaitsNormal)
	log.Printf("Performance:")
	log.Printf("  - Avg Processing Time (Overall): %.2f ms", avgProcessingTime)
	log.Printf("  - Avg Email Processing Time: %.2f ms", avgEmailProcessingTime)
	log.Printf("  - Avg Push Processing Time: %.2f ms", avgPushProcessingTime)
	log.Println("====================================")
}

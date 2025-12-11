package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	"golang.org/x/time/rate"

	"go-notifications-worker/internal/config"
	"go-notifications-worker/internal/connections"
	"go-notifications-worker/internal/models"
	"go-notifications-worker/internal/services"
	"go-notifications-worker/internal/worker"
	"go-notifications-worker/internal/constants"
)

func main() {
	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Println("Worker starting, id=", config.WorkerId)

	// Create metrics instance
	metrics := services.NewMetrics()

	limiterHigh := rate.NewLimiter(rate.Limit(config.HighPriorityRateLimit), config.HighPriorityRateLimit)       // 70% for high priority
	limiterNormal := rate.NewLimiter(rate.Limit(config.NormalPriorityRateLimit), config.NormalPriorityRateLimit) // 30% for normal priority

	// Start metrics logger (logs every 30 seconds)
	go services.LogMetricsPeriodically(ctx, metrics, 30*time.Second)

	// Start health check server
	go services.StartHealthCheckServer(metrics)

	// Connect to Db
	db := connections.InitDB(ctx)
	defer db.Close()

	// Connect to Firebase
	fcmClient := connections.InitFirebase(ctx)

	log.Println("worker started, id=", config.WorkerId)

	highCh := make(chan models.Notification, 5000)
	normalCh := make(chan models.Notification, 10000)

	var wg sync.WaitGroup

	// Start workers for HIGH priority
	for i := 0; i < config.HighPriorityWorkerPoolSize; i++ {
		wg.Add(1)
		go worker.Worker(ctx, &wg, highCh, db, fcmClient, limiterHigh, metrics, constants.PriorityHigh) // 1 = high priority
	}

	// Start workers for NORMAL priority
	for i := 0; i < config.NormalPriorityWorkerPoolSize; i++ {
		wg.Add(1)
		go worker.Worker(ctx, &wg, normalCh, db, fcmClient, limiterNormal, metrics, constants.PriorityNormal) // 2 = normal priority
	}

	// Start reaper
	go worker.ReaperLoop(ctx, db)

	log.Println("Worker started...")

	// Main loop polls DB every second
	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down...")
			close(highCh)
			close(normalCh)
			wg.Wait()
			log.Println("Shut down")
			return

		default:
			// Fetch batches
			high, err := worker.FetchNotifications(db, config.ClaimBatchHigh, 1)
			if err == nil {
				if len(high) > 0 {
					log.Printf("Fetched %d HIGH priority notifications\n", len(high))
				}
				for _, n := range high {
					highCh <- n
				}
			} else {
				log.Printf("Error fetching HIGH priority: %v\n", err)
				metrics.DatabaseErrors.Add(1)
			}

			normal, err := worker.FetchNotifications(db, config.ClaimBatchNormal, 2)
			if err == nil {
				if len(normal) > 0 {
					log.Printf("Fetched %d NORMAL priority notifications\n", len(normal))
				}
				for _, n := range normal {
					normalCh <- n
				}
			} else {
				log.Printf("Error fetching NORMAL priority: %v\n", err)
				metrics.DatabaseErrors.Add(1)
			}

			// Sleep if doing nothing
			if len(high) == 0 && len(normal) == 0 {
				time.Sleep(1 * time.Second)
			}
		}
	}
}

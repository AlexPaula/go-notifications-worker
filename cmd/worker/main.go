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
	"go-notifications-worker/internal/constants"
	"go-notifications-worker/internal/integrations"
	"go-notifications-worker/internal/models"
	"go-notifications-worker/internal/services"
	"go-notifications-worker/internal/worker"
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

	// Connect to Db
	db := integrations.InitDB(ctx)
	defer db.Close()

	// Connect to Firebase
	_, fcmClient := integrations.InitFirebase(ctx)

	// Connect to SMTP
	if err := integrations.InitSMTP(ctx); err != nil {
		log.Fatal("Failed to initialize SMTP pool:", err)
	}

	// Start metrics logger (logs every 30 seconds)
	go services.LogMetricsPeriodically(ctx, metrics, 30*time.Second)

	// Start health check server
	go services.StartHealthCheckServer(metrics)

	log.Println("worker started, id=", config.WorkerId)

	highCh := make(chan models.Notification, 5000)
	normalCh := make(chan models.Notification, 10000)

	var wg sync.WaitGroup

	// Start workers for HIGH priority
	for i := 0; i < config.HighPriorityWorkerPoolSize; i++ {
		wg.Add(1)
		go worker.SendNotifications(ctx, &wg, highCh, db, fcmClient, limiterHigh, metrics, constants.PriorityHigh) // 1 = high priority
	}

	// Start workers for NORMAL priority
	for i := 0; i < config.NormalPriorityWorkerPoolSize; i++ {
		wg.Add(1)
		go worker.SendNotifications(ctx, &wg, normalCh, db, fcmClient, limiterNormal, metrics, constants.PriorityNormal) // 2 = normal priority
	}

	// Start reaper
	go worker.ReaperLoop(ctx, db)

	log.Println("Worker started...")

	// Main loop polls DB every second
	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down...")

			// Close channels first to signal workers
			close(highCh)
			close(normalCh)

			// Wait for all workers to finish processing
			log.Println("Waiting for workers to complete...")
			wg.Wait()
			log.Println("All workers completed")

			// Close SMTP pool after workers are done
			integrations.SMTPClosePool()

			// Give DB a moment to flush any pending operations
			// db.Close() via defer will handle the rest gracefully
			time.Sleep(100 * time.Millisecond)

			log.Println("Shut down complete")
			return

		default:
			// Fetch batches
			high, err := integrations.DbFetchNotifications(ctx, db, config.ClaimBatchHigh, 1)
			if err == nil {
				if len(high) > 0 {
					log.Printf("Fetched %d HIGH priority notifications\n", len(high))
				}
				for _, n := range high {
					// Non-blocking send with backpressure handling
					select {
					case highCh <- n:
						// Successfully queued
					case <-ctx.Done():
						// Shutdown in progress - notification stays 'processing', reaper will reclaim it
						log.Printf("Shutdown: leaving notification %d for reaper\n", n.Id)
						return
					default:
						// Channel full, reschedule immediately to avoid blocking
						integrations.DbScheduleRetry(ctx, db, n.Id, 0)
						log.Printf("HIGH priority channel full, rescheduled notification %d\n", n.Id)
					}
				}
			} else {
				log.Printf("Error fetching HIGH priority: %v\n", err)
				metrics.DatabaseErrors.Add(1)
			}

			normal, err := integrations.DbFetchNotifications(ctx, db, config.ClaimBatchNormal, 2)
			if err == nil {
				if len(normal) > 0 {
					log.Printf("Fetched %d NORMAL priority notifications\n", len(normal))
				}
				for _, n := range normal {
					// Non-blocking send with backpressure handling
					select {
					case normalCh <- n:
						// Successfully queued
					case <-ctx.Done():
						// Shutdown in progress - notification stays 'processing', reaper will reclaim it
						log.Printf("Shutdown: leaving notification %d for reaper\n", n.Id)
						return
					default:
						// Channel full, reschedule immediately to avoid blocking
						integrations.DbScheduleRetry(ctx, db, n.Id, 0)
						log.Printf("NORMAL priority channel full, rescheduled notification %d\n", n.Id)
					}
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

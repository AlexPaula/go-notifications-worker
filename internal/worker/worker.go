package worker

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync"
	"time"

	"firebase.google.com/go/v4/messaging"
	"golang.org/x/time/rate"

	"go-notifications-worker/internal/config"
	"go-notifications-worker/internal/constants"
	"go-notifications-worker/internal/integrations"
	"go-notifications-worker/internal/models"
	"go-notifications-worker/internal/services"
)

// reaperLoop periodically finds expired leases and recovers them (blocked on state 'processing')
func ReaperLoop(ctx context.Context, db *sql.DB) {
	t := time.NewTicker(time.Duration(config.ReaperIntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("ReaperLoop shutting down")
			return
		case <-t.C:
			if err := integrations.DbReapExpired(ctx, db); err != nil {
				log.Printf("reaper error: %v", err)
			}
		}
	}
}

func SendNotifications(ctx context.Context, wg *sync.WaitGroup, ch <-chan models.Notification, db *sql.DB, fcm *messaging.Client, limiter *rate.Limiter, metrics *services.Metrics, priority int) {
	defer wg.Done()

	// Create a push batcher for this worker
	pushBatcher := integrations.FbNewPushBatcher(ctx, fcm, db, metrics)
	defer pushBatcher.Shutdown() // Shutdown batcher and wait for in-flight messages

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker %s (priority=%d) shutting down\n", config.WorkerId, priority)
			return

		case n, ok := <-ch:
			if !ok {
				log.Printf("worker %s (priority=%d) channel closed\n", config.WorkerId, priority)
				pushBatcher.Shutdown()
				return
			}

			startTime := time.Now()
			log.Printf("Processing %d (%s)\n", n.Id, n.Type)

			// Limit the rate globally
			if err := limiter.Wait(ctx); err != nil {
				log.Printf("Rate limiter error for notification %d: %v\n", n.Id, err)
				metrics.RateLimitErrors.Add(1)
				continue
			}

			// Track rate limit waits
			if priority == constants.PriorityHigh {
				metrics.RateLimitWaitsHigh.Add(1)
			} else {
				metrics.RateLimitWaitsNormal.Add(1)
			}

			var err error
			var sendStartTime time.Time
			switch strings.ToLower(n.Type) {
			case constants.NotificationTypeEmail:
				sendStartTime = time.Now()
				// Detect if body contains HTML tags
				isHtml := strings.Contains(n.Body.String, "<") && strings.Contains(n.Body.String, ">")
				err = integrations.SendEmail(ctx, n.To, "", "", n.Subject.String, n.Body.String, isHtml)
				sendDuration := time.Since(sendStartTime).Milliseconds()
				metrics.TotalEmailProcessingTimeMs.Add(sendDuration)
				if err != nil {
					metrics.EmailErrors.Add(1)
					log.Printf("Email FAILED - ID: %d, To: %s, Duration: %dms, Error: %v\n", n.Id, n.To, sendDuration, err)
				} else {
					metrics.EmailsSent.Add(1)
					log.Printf("Email SENT - ID: %d, To: %s, Duration: %dms\n", n.Id, n.To, sendDuration)
				}

			case constants.NotificationTypePush:
				// Queue push message for batching instead of sending immediately
				pushMsg := models.PushMessage{
					NotificationId: n.Id,
					Token:          n.To,
					Body:           n.Body.String,
					Title:          n.Subject.String,
					RetryCount:     n.RetryCount,
					MaxRetries:     n.MaxRetries,
					Priority:       n.Priority,
				}
				pushBatcher.Add(pushMsg)
			}

			if strings.ToLower(n.Type) == constants.NotificationTypeEmail {

				if err != nil {
					log.Printf("ERROR sending %d: %v\n", n.Id, err)

					if n.RetryCount+1 < n.MaxRetries {
						retryBackoffSec := config.RetryNormalBackoffSeconds
						if n.Priority == constants.PriorityHigh {
							retryBackoffSec = config.RetryHighBackoffSeconds
						}
						backoffSec := int(retryBackoffSec * (1 << n.RetryCount)) // 30s,60s,120s,...
						if backoffSec > 3600 {
							backoffSec = 3600
						}

						integrations.DbScheduleRetry(ctx, db, n.Id, backoffSec)
						log.Printf("Scheduled retry for notification %d (transient error)\n", n.Id)
					} else {
						integrations.DbSetErrorStatusAndAdd1RetryCount(ctx, db, n.Id)
						log.Printf("Max retries exceeded for notification %d\n", n.Id)
					}
				} else {
					integrations.DbSetStatus(ctx, db, n.Id, constants.NotificationStateSent)
				}

				processingTime := time.Since(startTime).Milliseconds()
				metrics.TotalProcessingTimeMs.Add(processingTime)

				if priority == 1 {
					metrics.HighPriorityProcessed.Add(1)
				} else {
					metrics.NormalPriorityProcessed.Add(1)
				}
			}
		}
	}
}

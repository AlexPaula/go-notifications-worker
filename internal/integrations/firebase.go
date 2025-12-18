package integrations

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"

	"go-notifications-worker/internal/config"
	"go-notifications-worker/internal/constants"
	"go-notifications-worker/internal/models"
	"go-notifications-worker/internal/services"
)

// PushBatcher accumulates notifications and sends them in optimized batches
// to reduce overhead while maintaining low latency
type PushBatcher struct {
	ctx       context.Context
	client    *messaging.Client
	db        *sql.DB
	metrics   *services.Metrics
	timeout   time.Duration
	batchSize int

	mu           sync.Mutex
	batch        []models.PushMessage
	batchTimer   *time.Timer
	wg           sync.WaitGroup // Track in-flight goroutines
	shuttingDown bool           // Prevent new messages during shutdown
}

func InitFirebase(ctx context.Context) (*firebase.App, *messaging.Client) {
	// Connect Firebase
	app, err := firebase.NewApp(ctx, nil, option.WithCredentialsFile(config.FirebaseCredentialsFile))
	if err != nil {
		log.Fatal("Firebase ERR:", err)
	}
	fcmClient, err := app.Messaging(ctx)
	if err != nil {
		log.Fatal("FCM ERR:", err)
	}
	log.Println("Firebase connected")
	return app, fcmClient
}

func FbNewPushBatcher(ctx context.Context, client *messaging.Client, db *sql.DB, metrics *services.Metrics) *PushBatcher {
	return &PushBatcher{
		ctx:       ctx,
		client:    client,
		db:        db,
		metrics:   metrics,
		timeout:   time.Duration(config.PushBatchTimeout) * time.Millisecond,
		batchSize: config.PushBatchSize,
		batch:     make([]models.PushMessage, 0, config.PushBatchSize),
	}
}

// Add queues a message for batch sending
func (pb *PushBatcher) Add(msg models.PushMessage) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	// Add validation here
	if msg.Token == "" {
		log.Printf("Rejecting message %d - empty token\n", msg.NotificationId)
		DbSetErrorStatusAndAdd1RetryCount(pb.ctx, pb.db, msg.NotificationId)
		return
	}

	// Reject new messages during shutdown
	if pb.shuttingDown {
		log.Printf("Rejecting message %d - batcher is shutting down\n", msg.NotificationId)
		return
	}

	pb.batch = append(pb.batch, msg)

	// Immediately count as processed (batching is async)
	if msg.Priority == constants.PriorityHigh {
		pb.metrics.HighPriorityProcessed.Add(1)
	} else {
		pb.metrics.NormalPriorityProcessed.Add(1)
	}

	// Start timer on first message
	if len(pb.batch) == 1 {
		pb.batchTimer = time.AfterFunc(pb.timeout, pb.Flush)
	}

	// Flush immediately if batch reaches threshold
	if len(pb.batch) >= pb.batchSize {
		if pb.batchTimer != nil {
			pb.batchTimer.Stop()
			pb.batchTimer = nil
		}
		pb.flushLocked()
	}
}

// Flush sends all queued messages (used during normal operation)
func (pb *PushBatcher) Flush() {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	if len(pb.batch) > 0 {
		pb.flushLocked()
	}
}

// Shutdown gracefully shuts down the batcher, flushing pending messages and waiting for completion
func (pb *PushBatcher) Shutdown() {
	pb.mu.Lock()
	// Mark as shutting down to prevent new messages
	pb.shuttingDown = true

	// Stop any active timer
	if pb.batchTimer != nil {
		pb.batchTimer.Stop()
		pb.batchTimer = nil
	}

	if len(pb.batch) > 0 {
		pb.flushLocked()
	}
	pb.mu.Unlock()

	// Wait for all in-flight goroutines to complete
	pb.wg.Wait()
	log.Println("PushBatcher: All in-flight messages completed")
}

// flushLocked sends the batch (caller must hold lock)
func (pb *PushBatcher) flushLocked() {
	if len(pb.batch) == 0 {
		return
	}

	messagesToSend := pb.batch
	pb.batch = make([]models.PushMessage, 0, pb.batchSize)

	if pb.batchTimer != nil {
		pb.batchTimer.Stop()
		pb.batchTimer = nil
	}

	// Send batch asynchronously to avoid blocking
	pb.wg.Add(1)
	go pb.sendBatch(messagesToSend)
}

func (pb *PushBatcher) sendBatch(messages []models.PushMessage) {
	defer pb.wg.Done()

	if len(messages) == 0 {
		return
	}

	// Check if context is already cancelled
	if err := pb.ctx.Err(); err != nil {
		return
	}

	startTime := time.Now()
	log.Printf("Sending batch of %d push notifications\n", len(messages))

	// Build Firebase messages
	fcmMessages := make([]*messaging.Message, 0, len(messages))
	for _, msg := range messages {
		fcmMsg := &messaging.Message{
			Token: msg.Token,
			Notification: &messaging.Notification{
				Title: msg.Title,
				Body:  msg.Body,
			},
			Android: &messaging.AndroidConfig{
				Priority: func() string {
					if msg.Priority == constants.PriorityHigh {
						return "high"
					}
					return "normal"
				}(),
			},
			APNS: &messaging.APNSConfig{
				Headers: map[string]string{
					"apns-priority": func() string {
						if msg.Priority == constants.PriorityHigh {
							return "10"
						}
						return "5"
					}(),
				},
			},
		}
		fcmMessages = append(fcmMessages, fcmMsg)
	}

	// Use SendAll for efficient batch sending (native Firebase batching)
	resp, err := pb.client.SendEach(pb.ctx, fcmMessages)

	batchDuration := time.Since(startTime).Milliseconds()
	pb.metrics.TotalPushProcessingTimeMs.Add(batchDuration)

	successCount := 0
	failureCount := 0

	if err != nil {
		log.Printf("Batch SendAll error: %v\n", err)
		// Check if error is due to context cancellation
		if pb.ctx.Err() != nil {
			return
		}
		pb.metrics.PushErrors.Add(int64(len(messages)))
		// Mark all as error and retry eligible ones
		for _, msg := range messages {
			pb.handleFailedMessage(msg, err)
		}
		log.Printf("Batch send failed: all %d messages marked for retry (total: %dms)\n", len(messages), batchDuration)
		return
	}

	// Process individual results from SendAll
	for idx, result := range resp.Responses {
		msg := messages[idx]

		if result.Error != nil {
			failureCount++
			pb.handleFailedMessage(msg, result.Error)
		} else {
			successCount++
			pb.metrics.PushSent.Add(1)
			DbSetStatus(pb.ctx, pb.db, msg.NotificationId, constants.NotificationStateSent)
			log.Printf("Push SENT (batch) - ID: %d, Token: %s\n", msg.NotificationId, msg.Token)
		}
	}

	log.Printf("Batch send complete: %d sent, %d failed (total: %dms)\n", successCount, failureCount, batchDuration)
}

func (pb *PushBatcher) handleFailedMessage(msg models.PushMessage, err error) {
	pb.metrics.PushErrors.Add(1)
	log.Printf("Push FAILED (batch) - ID: %d, Token: %s, Error: %v\n", msg.NotificationId, msg.Token, err)

	// Check if error is retryable
	isRetryable := messaging.IsQuotaExceeded(err) ||
		messaging.IsInternal(err) ||
		messaging.IsUnavailable(err) ||
		messaging.IsSenderIDMismatch(err) ||
		messaging.IsThirdPartyAuthError(err)

	if isRetryable && msg.RetryCount+1 < msg.MaxRetries {
		// Schedule retry with exponential backoff
		retryBackoffSec := config.RetryNormalBackoffSeconds
		backoffSec := int(retryBackoffSec * (1 << msg.RetryCount)) // 30s,60s,120s,...
		if backoffSec > 3600 {
			backoffSec = 3600
		}

		DbScheduleRetry(pb.ctx, pb.db, msg.NotificationId, backoffSec)
		log.Printf("Scheduled retry for notification %d (transient error)\n", msg.NotificationId)

	} else {
		DbSetErrorStatusAndAdd1RetryCount(pb.ctx, pb.db, msg.NotificationId)
		log.Printf("Non-retryable or max retries exceeded for notification %d (retryCount=%d, maxRetries=%d) - marking as error\n", msg.NotificationId, msg.RetryCount, msg.MaxRetries)
	}
}

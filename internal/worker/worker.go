package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"firebase.google.com/go/messaging"
	"golang.org/x/time/rate"

	"go-notifications-worker/internal/config"
	"go-notifications-worker/internal/models"
	"go-notifications-worker/internal/services"
)

// ================================================================
// DB: Fetch pending notifications
// ================================================================

func FetchNotifications(db *sql.DB, limit int, priority int) ([]models.Notification, error) {
	// Start an explicit transaction
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // Rollback if we don't commit

	query := `
        SELECT TOP (@p1) Id, Type, Priority, [To], Subject, Body, RetryCount, MaxRetries
        FROM NotificationJournal WITH (ROWLOCK, READPAST, UPDLOCK)
        WHERE Status = 'pending' AND Priority = @p2 AND (NextAttemptAt IS NULL OR NextAttemptAt <= GETUTCDATE())
        ORDER BY CreatedAt
    `

	rows, err := tx.Query(query, limit, priority)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.Notification
	var ids []int64

	for rows.Next() {
		var n models.Notification
		err := rows.Scan(&n.Id, &n.Type, &n.Priority, &n.To, &n.Subject, &n.Body, &n.RetryCount, &n.MaxRetries)
		if err != nil {
			return nil, err
		}
		list = append(list, n)
		ids = append(ids, n.Id)
	}
	rows.Close() // Close rows before executing update

	// If we got any notifications, mark them as 'processing' to prevent re-fetching
	if len(ids) > 0 {
		// Build a comma-separated list of IDs for the IN clause
		idList := ""
		for i, id := range ids {
			if i > 0 {
				idList += ","
			}
			idList += fmt.Sprintf("%d", id)
		}

		updateQuery := fmt.Sprintf(`UPDATE NotificationJournal 
			SET Status='processing', UpdatedAt=GETUTCDATE() 
			WHERE Id IN (%s)`, idList)

		_, err = tx.Exec(updateQuery)
		if err != nil {
			return nil, err
		}
	}

	// Commit the transaction to release locks
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return list, nil
}

func SetStatus(db *sql.DB, id int64, status string) {
	_, err := db.Exec(`
		UPDATE NotificationJournal 
		SET Status=@p1, 
			UpdatedAt=GETUTCDATE() 
		WHERE Id=@p2 
			AND (Status = 'processing' OR @p1 = 'sent')`,
		status, id)

	log.Printf("set status err id=%d: %v", id, err)
}

// reaperLoop periodically finds expired leases and recovers them (blocked on state 'processing')
func ReaperLoop(ctx context.Context, db *sql.DB) {
	t := time.NewTicker(time.Duration(config.ReaperIntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ReapExpired(ctx, db); err != nil {
				log.Printf("reaper error: %v", err)
			}
		}
	}
}

// reapExpired resets rows to be processed again
func ReapExpired(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status = 'pending',
			UpdatedAt = GETUTCDATE()
		WHERE Status = 'processing'
			AND UpdatedAt >= DATEADD(SECOND, @backoff, UpdatedAt)
	`, sql.Named("backoff", config.ReclaimBackoffSeconds))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("reaper: reset %d (to pending)\n", n)
	}

	return nil
}

func ScheduleRetry(db *sql.DB, id int64, backoffSec int) {
	_, err := db.Exec(`
		UPDATE NotificationJournal 
		SET Status = 'pending', 
			RetryCount = RetryCount + 1,
			NextAttemptAt = DATEADD(SECOND, @p1, GETUTCDATE()),
			UpdatedAt=GETUTCDATE() 
			WHERE Id=@p2 
				AND Status = 'processing'`,
		backoffSec, id)

	log.Printf("schedule retry err id=%d: %v", id, err)
}

// ================================================================
// Worker
// ================================================================

func Worker(ctx context.Context, wg *sync.WaitGroup, ch <-chan models.Notification, db *sql.DB, fcm *messaging.Client, limiter *rate.Limiter, metrics *services.Metrics, priority int) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker %s shutting down\n", config.WorkerId)
			return

		case n, ok := <-ch:
			if !ok {
				return
			}

			startTime := time.Now()
			log.Printf("Processing %d (%s)\n", n.Id, n.Type)

			// Limit the rate globally
			if err := limiter.Wait(ctx); err != nil {
				log.Printf("Rate limiter error for notification %d: %v\n", n.Id, err)
				metrics.RateLimitErrors.Add(1)
				return // Graceful shutdown
			}

			// Track rate limit waits
			if priority == 1 {
				metrics.RateLimitWaitsHigh.Add(1)
			} else {
				metrics.RateLimitWaitsNormal.Add(1)
			}

			var err error
			switch strings.ToLower(n.Type) {
			case "email":
				err = SendEmail(n.To, "", "", n.Subject.String, n.Body.String)
				if err != nil {
					metrics.EmailErrors.Add(1)
				} else {
					metrics.EmailsSent.Add(1)
				}
			case "push":
				err = SendPush(ctx, fcm, n.To, n.Body.String)
				if err != nil {
					metrics.PushErrors.Add(1)
				} else {
					metrics.PushSent.Add(1)
				}
			}

			if err != nil {
				log.Printf("ERROR sending %d: %v\n", n.Id, err)

				if n.RetryCount+1 >= n.MaxRetries {
					SetStatus(db, n.Id, "error")
				} else {
					// schedule retry with exponential backoff (simple)
					retryBackoffSec := config.RetryNormalBackoffSeconds
					if n.Priority == 1 {
						retryBackoffSec = config.RetryHighBackoffSeconds
					}
					backoffSec := int(retryBackoffSec * (1 << n.RetryCount)) // 30s,60s,120s,...
					if backoffSec > 3600 {
						backoffSec = 3600
					}
					ScheduleRetry(db, n.Id, backoffSec)
				}
			} else {
				SetStatus(db, n.Id, "sent")
			}

			// Track processing metrics
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

// ================================================================
// Email Sending (MailKit-like via SMTP)
// ================================================================

func SendEmail(to, cc, bcc, subject, body string) error {
	auth := smtp.PlainAuth("", config.SmtpFrom, config.SmtpPassword, config.SmtpHost)

	msg := "From: " + config.SmtpFrom + "\r\n" +
		"To: " + to + "\r\n" +
		"Cc: " + cc + "\r\n" +
		"Bcc: " + bcc + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body + "\r\n"

	err := smtp.SendMail(
		config.SmtpHost+":"+config.SmtpPort,
		auth,
		config.SmtpFrom,
		[]string{to},
		[]byte(msg),
	)

	return err
}

// ================================================================
// Firebase Push
// ================================================================

func SendPush(ctx context.Context, client *messaging.Client, token, body string) error {
	message := &messaging.Message{
		Token: token,
		Notification: &messaging.Notification{
			Title: "Notification",
			Body:  body,
		},
	}
	_, err := client.Send(ctx, message)
	return err
}

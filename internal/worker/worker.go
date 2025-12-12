package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"firebase.google.com/go/messaging"
	"golang.org/x/time/rate"

	"go-notifications-worker/internal/config"
	"go-notifications-worker/internal/connections"
	"go-notifications-worker/internal/constants"
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
			AND Status != 'sent'`,
		status, id)

	if err != nil {
		log.Printf("set status err id=%d: %v", id, err)
	}
}

func SetErrorStatusAndAdd1RetryCount(db *sql.DB, id int64) {
	_, err := db.Exec(`
		UPDATE NotificationJournal 
		SET Status=@p1, 
			RetryCount = RetryCount + 1,
			UpdatedAt=GETUTCDATE() 
		WHERE Id=@p2 
			AND Status != 'sent'`,
		constants.NotificationStateError, id)

	if err != nil {
		log.Printf("set status err id=%d: %v", id, err)
	}
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
			AND UpdatedAt <= DATEADD(SECOND, -@backoff, GETUTCDATE())
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
				AND (Status = 'processing' OR Status = 'pending')`,
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
				err = SendEmail(ctx, n.To, "", "", n.Subject.String, n.Body.String, isHtml)
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
				sendStartTime = time.Now()
				err = SendPush(ctx, fcm, n.To, n.Body.String)
				sendDuration := time.Since(sendStartTime).Milliseconds()
				metrics.TotalPushProcessingTimeMs.Add(sendDuration)
				if err != nil {
					metrics.PushErrors.Add(1)
					log.Printf("Push FAILED - ID: %d, Token: %s, Duration: %dms, Error: %v\n", n.Id, n.To, sendDuration, err)
				} else {
					metrics.PushSent.Add(1)
					log.Printf("Push SENT - ID: %d, Token: %s, Duration: %dms\n", n.Id, n.To, sendDuration)
				}
			}

			if err != nil {
				log.Printf("ERROR sending %d: %v\n", n.Id, err)

				// Check if error is retryable (for both email and push)
				isRetryable := strings.Contains(err.Error(), constants.FCMError_Retry) ||
					(strings.ToLower(n.Type) == constants.NotificationTypeEmail) // Emails are generally retryable

				if isRetryable && n.RetryCount+1 < n.MaxRetries {
					// Schedule retry with exponential backoff for transient errors
					retryBackoffSec := config.RetryNormalBackoffSeconds
					if n.Priority == constants.PriorityHigh {
						retryBackoffSec = config.RetryHighBackoffSeconds
					}
					backoffSec := int(retryBackoffSec * (1 << n.RetryCount)) // 30s,60s,120s,...
					if backoffSec > 3600 {
						backoffSec = 3600
					}

					ScheduleRetry(db, n.Id, backoffSec)
					log.Printf("Scheduled retry for notification %d (transient error)\n", n.Id)

				} else if strings.Contains(err.Error(), constants.FCMError_InvalidToken) ||
					strings.Contains(err.Error(), constants.FCMError_InvalidArgument) {

					SetStatus(db, n.Id, constants.NotificationStateError)
					log.Printf("Non-retryable error for notification %d - marking as failed\n", n.Id)

				} else if !isRetryable || n.RetryCount+1 >= n.MaxRetries {
					// Max retries exceeded or non-retryable
					SetErrorStatusAndAdd1RetryCount(db, n.Id)
					log.Printf("Max retries exceeded or non-retryable error for notification %d (retryCount=%d, maxRetries=%d)\n", n.Id, n.RetryCount, n.MaxRetries)
				}
			} else {
				SetStatus(db, n.Id, constants.NotificationStateSent)
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
// Email Sending (via SMTP)
// ================================================================

// stripHTMLTags removes HTML tags from a string, returning plain text
func stripHTMLTags(html string) string {
	// Remove script and style elements
	re := regexp.MustCompile(`(?s)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>`)
	text := re.ReplaceAllString(html, "")

	// Remove all HTML tags
	re = regexp.MustCompile(`<[^>]*>`)
	text = re.ReplaceAllString(text, "")

	// Decode HTML entities and collapse whitespace
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)

	return text
}

func SendEmail(ctx context.Context, to, cc, bcc, subject, body string, isBodyHtml bool) error {
	// Get connection with context timeout (fails fast if no connections available)
	client, err := connections.GetSMTPConnection(ctx)
	if err != nil {
		return fmt.Errorf("failed to get SMTP connection: %w", err)
	}

	isHealthy := true
	defer func() {
		connections.ReturnSMTPConnection(client, isHealthy)
	}()

	// Build message headers (CC visible, BCC NOT included)
	msg := "From: " + config.SmtpFrom + "\r\n" +
		"To: " + to + "\r\n"

	if cc != "" {
		msg += "Cc: " + cc + "\r\n"
	}
	// NOTE: Do NOT add Bcc to headers - it's hidden!

	msg += "Subject: " + subject + "\r\n"

	// Build message body based on content type
	if isBodyHtml {
		// Use MIME multipart for HTML email with plain text fallback
		boundary := "===============" + fmt.Sprintf("%d", time.Now().UnixNano()) + "==============="
		msg += "MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n" +
			"\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: 7bit\r\n" +
			"\r\n" +
			stripHTMLTags(body) + "\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: 7bit\r\n" +
			"\r\n" +
			body + "\r\n" +
			"--" + boundary + "--\r\n"
	} else {
		// Simple plain text email
		msg += "Content-Type: text/plain; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: 7bit\r\n" +
			"\r\n" +
			body + "\r\n"
	}

	// Build SMTP recipients list (includes To, Cc, AND Bcc)
	recipients := []string{to}
	if cc != "" {
		recipients = append(recipients, strings.Split(cc, ",")...)
	}
	if bcc != "" {
		recipients = append(recipients, strings.Split(bcc, ",")...)
	}

	// Clean up whitespace in recipient emails
	for i := range recipients {
		recipients[i] = strings.TrimSpace(recipients[i])
	}

	// Send mail

	/*
	   The SMTP sequence is:

	   Mail()  - Tell the server who the email is FROM
	   Rcpt()  - Tell the server who to send TO (called once per recipient)
	   Data()  - Send the actual message content (Opens a write channel to send the message content)
	   Write() - Writes the actual email message (headers + body) to that channel

	   So the Rcpt() loop tells the server all the addresses that should receive this email (To + Cc + Bcc),
	   and then Data() sends the actual message body. The server handles delivering to all those recipients.
	*/

	if err := client.Mail(config.SmtpFrom); err != nil {
		isHealthy = false
		return err
	}

	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			isHealthy = false
			return err
		}
	}

	wc, err := client.Data()
	if err != nil {
		isHealthy = false
		return err
	}
	defer wc.Close()

	_, err = wc.Write([]byte(msg))
	if err != nil {
		isHealthy = false
	}
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

	if err != nil {
		// Check if error is retryable
		if messaging.IsRegistrationTokenNotRegistered(err) {
			return fmt.Errorf("%s: %w", constants.FCMError_InvalidToken, err)
		} else if messaging.IsInvalidArgument(err) {
			return fmt.Errorf("%s: %w", constants.FCMError_InvalidArgument, err)
		}
		if messaging.IsUnknown(err) ||
			messaging.IsMessageRateExceeded(err) ||
			messaging.IsInternal(err) ||
			messaging.IsServerUnavailable(err) {
			return fmt.Errorf("%s: %w", constants.FCMError_Retry, err)
		}
	}

	return err
}

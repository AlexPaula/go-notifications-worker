package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/smtp"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	firebase "firebase.google.com/go"
	"firebase.google.com/go/messaging"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	_ "github.com/microsoft/go-mssqldb"
	"golang.org/x/time/rate"
	"google.golang.org/api/option"
)

type Notification struct {
	Id         int64
	Type       string // email | push
	Priority   int    // 1=high, 2=normal
	To         string
	Subject    sql.NullString
	Body       sql.NullString
	RetryCount int
	MaxRetries int
}

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
	TotalProcessingTimeMs atomic.Int64
	StartTime             time.Time
}

// Config / tuning - defaults
const (
	defaultFirebaseCredentialsFile      = "firebase-adminsdk.json"
	defaultClaimBatchHigh               = 200
	defaultClaimBatchNormal             = 500
	defaultHighPriorityWorkerPoolSize   = 200
	defaultNormalPriorityWorkerPoolSize = 200
	defaultHighPriorityRateLimit        = 700
	defaultNormalPriorityRateLimit      = 300
)

// Configuration loaded from environment
var (
	sqlConnString                string
	claimBatchHigh               int
	claimBatchNormal             int
	highPriorityWorkerPoolSize   int
	normalPriorityWorkerPoolSize int
	highPriorityRateLimit        int
	normalPriorityRateLimit      int
	smtpFrom                     string
	smtpPassword                 string
	smtpHost                     string
	smtpPort                     string
	firebaseCredentialsFile      string
	healthCheckPort              string
)

// workerId unique for this process
var workerId string

func init() {
	workerId = fmt.Sprintf("%s-%d", uuid.New().String(), os.Getpid())

	// Load environment variables from .env file
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found, using environment variables or defaults")
	}

	// Load configuration
	loadConfig()
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt retrieves an integer environment variable or returns a default value
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
		log.Printf("Warning: Invalid integer value for %s, using default %d", key, defaultValue)
	}
	return defaultValue
}

// loadConfig loads all configuration from environment variables
func loadConfig() {
	healthCheckPort = getEnv("HEALTH_CHECK_PORT", "8080")

	sqlConnString = getEnv("DB_CONNECTION_STRING", "")
	if sqlConnString == "" {
		log.Fatal("DB_CONNECTION_STRING environment variable is required")
	}

	// Email configuration
	smtpFrom = getEnv("SMTP_FROM", "")
	smtpPassword = getEnv("SMTP_PASSWORD", "")
	smtpHost = getEnv("SMTP_HOST", "")
	smtpPort = getEnv("SMTP_PORT", "25")

	if smtpFrom == "" || smtpPassword == "" || smtpHost == "" {
		log.Println("Warning: Email configuration incomplete. Email sending may fail.")
	}

	// Firebase configuration
	firebaseCredentialsFile = getEnv("FIREBASE_CREDENTIALS_FILE", defaultFirebaseCredentialsFile)

	// Worker configuration with defaults
	claimBatchHigh = getEnvInt("CLAIM_BATCH_HIGH", defaultClaimBatchHigh)
	claimBatchNormal = getEnvInt("CLAIM_BATCH_NORMAL", defaultClaimBatchNormal)
	highPriorityWorkerPoolSize = getEnvInt("HIGH_PRIORITY_WORKER_POOL_SIZE", defaultHighPriorityWorkerPoolSize)
	normalPriorityWorkerPoolSize = getEnvInt("NORMAL_PRIORITY_WORKER_POOL_SIZE", defaultNormalPriorityWorkerPoolSize)
	highPriorityRateLimit = getEnvInt("HIGH_PRIORITY_RATE_LIMIT", defaultHighPriorityRateLimit)
	normalPriorityRateLimit = getEnvInt("NORMAL_PRIORITY_RATE_LIMIT", defaultNormalPriorityRateLimit)

	log.Println("Configuration loaded successfully")
}

// logMetricsPeriodically logs metrics at regular intervals
func logMetricsPeriodically(ctx context.Context, m *Metrics, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Log final metrics before shutdown
			logMetrics(m)
			return
		case <-ticker.C:
			logMetrics(m)
		}
	}
}

// logMetrics outputs current metrics to the log
func logMetrics(m *Metrics) {
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
	log.Printf("  - Avg Processing Time: %.2f ms", avgProcessingTime)
	log.Println("====================================")
}

func main() {
	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Println("Worker starting, id=", workerId)

	limiterHigh := rate.NewLimiter(rate.Limit(highPriorityRateLimit), highPriorityRateLimit)       // 70% for high priority
	limiterNormal := rate.NewLimiter(rate.Limit(normalPriorityRateLimit), normalPriorityRateLimit) // 30% for normal priority

	// Initialize metrics
	metrics := &Metrics{
		StartTime: time.Now(),
	}

	// Start metrics logger (logs every 30 seconds)
	go logMetricsPeriodically(ctx, metrics, 30*time.Second)

	// Start health check server
	go startHealthCheckServer(metrics)

	// Connect SQL Server
	db, err := sql.Open("sqlserver", sqlConnString)
	if err != nil {
		log.Fatal("DB ERR:", err)
	}
	defer db.Close()
	log.Println("DB connected")

	// simple connection check
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}

	// Connect Firebase
	app, err := firebase.NewApp(ctx, nil, option.WithCredentialsFile(firebaseCredentialsFile))
	if err != nil {
		log.Fatal("FIREBASE ERR:", err)
	}
	fcmClient, err := app.Messaging(ctx)
	if err != nil {
		log.Fatal("FCM ERR:", err)
	}
	log.Println("Firebase connected")

	log.Println("worker started, id=", workerId)

	highCh := make(chan Notification, 5000)
	normalCh := make(chan Notification, 10000)

	var wg sync.WaitGroup

	// Start workers for HIGH priority
	for i := 0; i < highPriorityWorkerPoolSize; i++ {
		wg.Add(1)
		go worker(ctx, &wg, highCh, db, fcmClient, limiterHigh, metrics, 1) // 1 = high priority
	}

	// Start workers for NORMAL priority
	for i := 0; i < normalPriorityWorkerPoolSize; i++ {
		wg.Add(1)
		go worker(ctx, &wg, normalCh, db, fcmClient, limiterNormal, metrics, 2) // 2 = normal priority
	}

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
			high, err := fetchNotifications(db, claimBatchHigh, 1)
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

			normal, err := fetchNotifications(db, claimBatchNormal, 2)
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

			if len(high) == 0 && len(normal) == 0 {
				time.Sleep(1 * time.Second)
			}
		}
	}
}

// ================================================================
// DB: Fetch pending notifications
// ================================================================

func fetchNotifications(db *sql.DB, limit int, priority int) ([]Notification, error) {
	// Start an explicit transaction
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // Rollback if we don't commit

	query := `
        SELECT TOP (@p1) Id, Type, Priority, [To], Subject, Body, RetryCount, MaxRetries
        FROM NotificationJournal WITH (ROWLOCK, READPAST, UPDLOCK)
        WHERE Status = 'pending' AND Priority = @p2
        ORDER BY CreatedAt
    `

	rows, err := tx.Query(query, limit, priority)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Notification
	var ids []int64

	for rows.Next() {
		var n Notification
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
			SET Status='processing', UpdatedAt=GETDATE() 
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

func setStatus(db *sql.DB, id int64, status string) {
	_, _ = db.Exec(`UPDATE NotificationJournal SET Status=@p1, UpdatedAt=GETDATE() WHERE Id=@p2`,
		status, id)
}

// ================================================================
// Worker
// ================================================================

func worker(ctx context.Context, wg *sync.WaitGroup, ch <-chan Notification, db *sql.DB, fcm *messaging.Client, limiter *rate.Limiter, metrics *Metrics, priority int) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker %s shutting down\n", workerId)
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
				err = sendEmail(n.To, "", "", n.Subject.String, n.Body.String)
				if err != nil {
					metrics.EmailErrors.Add(1)
				} else {
					metrics.EmailsSent.Add(1)
				}
			case "push":
				err = sendPush(ctx, fcm, n.To, n.Body.String)
				if err != nil {
					metrics.PushErrors.Add(1)
				} else {
					metrics.PushSent.Add(1)
				}
			}

			if err != nil {
				log.Printf("ERROR sending %d: %v\n", n.Id, err)
				setStatus(db, n.Id, "error")
			} else {
				setStatus(db, n.Id, "sent")
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

func sendEmail(to, cc, bcc, subject, body string) error {
	auth := smtp.PlainAuth("", smtpFrom, smtpPassword, smtpHost)

	msg := "From: " + smtpFrom + "\r\n" +
		"To: " + to + "\r\n" +
		"Cc: " + cc + "\r\n" +
		"Bcc: " + bcc + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body + "\r\n"

	err := smtp.SendMail(
		smtpHost+":"+smtpPort,
		auth,
		smtpFrom,
		[]string{to},
		[]byte(msg),
	)

	return err
}

// ================================================================
// Firebase Push
// ================================================================

func sendPush(ctx context.Context, client *messaging.Client, token, body string) error {
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

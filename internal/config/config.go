package config

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

// Config / tuning - defaults
const (
	DefaultFirebaseCredentialsFile      = "firebase-adminsdk.json"
	DefaultClaimBatchHigh               = 200
	DefaultClaimBatchNormal             = 500
	DefaultHighPriorityWorkerPoolSize   = 200
	DefaultNormalPriorityWorkerPoolSize = 200
	DefaultHighPriorityRateLimit        = 700
	DefaultNormalPriorityRateLimit      = 300
	DefaultReaperIntervalSeconds        = 15
	DefaultReclaimBackoffSeconds        = 60
	DefaultRetryHighBackoffSeconds      = 15
	DefaultRetryNormalBackoffSeconds    = 30
	DefaultPushBatchTimeout             = 50  // milliseconds
	DefaultPushBatchSize                = 100 // max messages per batch
	DefaultPushBatchMaxSize             = 500 // Firebase FCM limit
	DefaultDbMaxOpenConns               = 150
	DefaultDbMaxIdleConns               = 20
	DefaultDbConnMaxLifetimeMinutes     = 30
	DefaultDbConnMaxIdleTimeMinutes     = 5
)

// Configuration loaded from environment
var (
	SqlConnString                string
	DbMaxOpenConns               int
	DbMaxIdleConns               int
	DbConnMaxLifetimeMinutes     int
	DbConnMaxIdleTimeMinutes     int
	ClaimBatchHigh               int
	ClaimBatchNormal             int
	HighPriorityWorkerPoolSize   int
	NormalPriorityWorkerPoolSize int
	HighPriorityRateLimit        int
	NormalPriorityRateLimit      int
	SmtpFrom                     string
	SmtpUsername                 string
	SmtpPassword                 string
	SmtpHost                     string
	SmtpPort                     string
	SmtpPoolSize                 int
	SmtpTLSMode                  string
	FirebaseCredentialsFile      string
	HealthCheckPort              string
	ReaperIntervalSeconds        int
	ReclaimBackoffSeconds        int
	RetryHighBackoffSeconds      int
	RetryNormalBackoffSeconds    int
	PushBatchTimeout             int
	PushBatchSize                int
	PushBatchMaxSize             int
)

// WorkerId unique for this process
var WorkerId string

// The Go runtime automatically calls all init() functions when a package is initialized
func init() {
	WorkerId = fmt.Sprintf("%s-%d", uuid.New().String(), os.Getpid())

	// Load environment variables from .env file
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found, using environment variables or defaults")
	}

	// Load configuration
	LoadConfig()
}

// GetEnv retrieves an environment variable or returns a default value
func GetEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// GetEnvInt retrieves an integer environment variable or returns a default value
func GetEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
		log.Printf("Warning: Invalid integer value for %s, using default %d", key, defaultValue)
	}
	return defaultValue
}

// LoadConfig loads all configuration from environment variables
func LoadConfig() {
	HealthCheckPort = GetEnv("HEALTH_CHECK_PORT", "8080")

	SqlConnString = GetEnv("DB_CONNECTION_STRING", "")
	if SqlConnString == "" {
		log.Fatal("DB_CONNECTION_STRING environment variable is required")
	}

	// Database connection pool configuration
	DbMaxOpenConns = GetEnvInt("DB_MAX_OPEN_CONNS", DefaultDbMaxOpenConns)
	DbMaxIdleConns = GetEnvInt("DB_MAX_IDLE_CONNS", DefaultDbMaxIdleConns)
	DbConnMaxLifetimeMinutes = GetEnvInt("DB_CONN_MAX_LIFETIME_MINUTES", DefaultDbConnMaxLifetimeMinutes)
	DbConnMaxIdleTimeMinutes = GetEnvInt("DB_CONN_MAX_IDLE_TIME_MINUTES", DefaultDbConnMaxIdleTimeMinutes)

	// Email configuration
	SmtpFrom = GetEnv("SMTP_FROM", "")
	SmtpUsername = GetEnv("SMTP_USERNAME", SmtpFrom) // Default to SMTP_FROM if not set
	SmtpPassword = GetEnv("SMTP_PASSWORD", "")
	SmtpHost = GetEnv("SMTP_HOST", "")
	SmtpPort = GetEnv("SMTP_PORT", "465")
	SmtpPoolSize = GetEnvInt("SMTP_POOL_SIZE", 5)
	// TLS mode: "tls" (implicit TLS/SMTPS, port 465), "starttls" (upgrade, port 587), "none" (insecure, port 25)
	SmtpTLSMode = GetEnv("SMTP_TLS_MODE", "tls")

	if SmtpFrom == "" || SmtpHost == "" {
		log.Println("Warning: Email configuration incomplete. Email sending may fail.")
	}

	if SmtpPassword == "" {
		log.Println("Info: SMTP password not set, will attempt relay mode (no authentication)")
	}

	// Warn about common port/TLS mode mismatches
	if SmtpTLSMode == "tls" && SmtpPort != "465" {
		log.Printf("Warning: SMTP_TLS_MODE is 'tls' but port is %s (typically use 465 for implicit TLS)", SmtpPort)
	} else if SmtpTLSMode == "starttls" && SmtpPort != "587" {
		log.Printf("Warning: SMTP_TLS_MODE is 'starttls' but port is %s (typically use 587 for STARTTLS)", SmtpPort)
	} else if SmtpTLSMode == "none" && (SmtpPort != "25" && SmtpPort != "2525") {
		log.Printf("Warning: SMTP_TLS_MODE is 'none' but port is %s (typically use 25 or 2525 for plain SMTP)", SmtpPort)
	}

	// Firebase configuration
	FirebaseCredentialsFile = GetEnv("FIREBASE_CREDENTIALS_FILE", DefaultFirebaseCredentialsFile)

	// Worker configuration with defaults
	ClaimBatchHigh = GetEnvInt("CLAIM_BATCH_HIGH", DefaultClaimBatchHigh)
	ClaimBatchNormal = GetEnvInt("CLAIM_BATCH_NORMAL", DefaultClaimBatchNormal)
	HighPriorityWorkerPoolSize = GetEnvInt("HIGH_PRIORITY_WORKER_POOL_SIZE", DefaultHighPriorityWorkerPoolSize)
	NormalPriorityWorkerPoolSize = GetEnvInt("NORMAL_PRIORITY_WORKER_POOL_SIZE", DefaultNormalPriorityWorkerPoolSize)
	HighPriorityRateLimit = GetEnvInt("HIGH_PRIORITY_RATE_LIMIT", DefaultHighPriorityRateLimit)
	NormalPriorityRateLimit = GetEnvInt("NORMAL_PRIORITY_RATE_LIMIT", DefaultNormalPriorityRateLimit)

	ReaperIntervalSeconds = GetEnvInt("REAPER_INTERVAL_SECONDS", DefaultReaperIntervalSeconds)
	ReclaimBackoffSeconds = GetEnvInt("RECLAIM_BACKOFF_SECONDS", DefaultReclaimBackoffSeconds)
	RetryHighBackoffSeconds = GetEnvInt("RETRY_HIGH_BACKOFF_SECONDS", DefaultRetryHighBackoffSeconds)
	RetryNormalBackoffSeconds = GetEnvInt("RETRY_NORMAL_BACKOFF_SECONDS", DefaultRetryNormalBackoffSeconds)

	// Push batching configuration
	PushBatchTimeout = GetEnvInt("PUSH_BATCH_TIMEOUT_MS", DefaultPushBatchTimeout)
	PushBatchSize = GetEnvInt("PUSH_BATCH_SIZE", DefaultPushBatchSize)
	PushBatchMaxSize = GetEnvInt("PUSH_BATCH_MAX_SIZE", DefaultPushBatchMaxSize)

	log.Println("Configuration loaded successfully")
}

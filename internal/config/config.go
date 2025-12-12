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
)

// Configuration loaded from environment
var (
	SqlConnString                string
	ClaimBatchHigh               int
	ClaimBatchNormal             int
	HighPriorityWorkerPoolSize   int
	NormalPriorityWorkerPoolSize int
	HighPriorityRateLimit        int
	NormalPriorityRateLimit      int
	SmtpFrom                     string
	SmtpPassword                 string
	SmtpHost                     string
	SmtpPort                     string
	SmtpPoolSize                 int
	FirebaseCredentialsFile      string
	HealthCheckPort              string
	ReaperIntervalSeconds        int
	ReclaimBackoffSeconds        int
	RetryHighBackoffSeconds      int
	RetryNormalBackoffSeconds    int
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

	// Email configuration
	SmtpFrom = GetEnv("SMTP_FROM", "")
	SmtpPassword = GetEnv("SMTP_PASSWORD", "")
	SmtpHost = GetEnv("SMTP_HOST", "")
	SmtpPort = GetEnv("SMTP_PORT", "25")
	SmtpPoolSize = GetEnvInt("SMTP_POOL_SIZE", 5)

	if SmtpFrom == "" || SmtpPassword == "" || SmtpHost == "" {
		log.Println("Warning: Email configuration incomplete. Email sending may fail.")
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
	RetryNormalBackoffSeconds = GetEnvInt("RETRY_NORMAL_BACKOFF_SECONDS_", DefaultRetryNormalBackoffSeconds)

	log.Println("Configuration loaded successfully")
}

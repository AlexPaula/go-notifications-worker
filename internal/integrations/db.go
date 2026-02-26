package integrations

import (
	"context"
	"database/sql"
	"log"
	"time"

	"go-notifications-worker/internal/config"
	"go-notifications-worker/internal/models"
)

// dbProvider abstracts all database operations so the rest of the codebase
// is driver-agnostic. Implementations live in db-sqlserver.go and db-postgres.go.
type dbProvider interface {
	driverName() string
	fetchNotifications(ctx context.Context, db *sql.DB, limit int, priority int) ([]models.Notification, error)
	setStatus(ctx context.Context, db *sql.DB, id int64, status string)
	setErrorStatusAndAdd1RetryCount(ctx context.Context, db *sql.DB, id int64)
	reapExpired(ctx context.Context, db *sql.DB) error
	scheduleRetry(ctx context.Context, db *sql.DB, id int64, backoffSec int)
}

// activeProvider is set once by InitDB and used by all public functions below.
var activeProvider dbProvider

// InitDB selects the appropriate driver based on DB_DRIVER config, opens the
// connection pool, and returns the *sql.DB for use throughout the application.
func InitDB(ctx context.Context) *sql.DB {
	switch config.DBDriver {
	case "postgres":
		activeProvider = &postgresProvider{}
		log.Println("DB driver: postgres (pgx)")
	default:
		activeProvider = &sqlServerProvider{}
		log.Println("DB driver: sqlserver")
	}

	db, err := sql.Open(activeProvider.driverName(), config.DBConnString)
	if err != nil {
		log.Fatal("DB ERR:", err)
	}

	// Connection pool tuning
	db.SetMaxOpenConns(config.DbMaxOpenConns)
	db.SetMaxIdleConns(config.DbMaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(config.DbConnMaxLifetimeMinutes) * time.Minute)
	db.SetConnMaxIdleTime(time.Duration(config.DbConnMaxIdleTimeMinutes) * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("DB ping failed: %v", err)
	}

	log.Println("DB connected successfully")
	return db
}

// activeReclaimBackoffSeconds is a helper used by both provider implementations.
func activeReclaimBackoffSeconds() int {
	return config.ReclaimBackoffSeconds
}

// ---------------------------------------------------------------------------
// Public API — callers (main.go, worker.go) use these functions.
// They delegate to the driver-specific implementation selected at startup.
// ---------------------------------------------------------------------------

func DbFetchNotifications(ctx context.Context, db *sql.DB, limit int, priority int) ([]models.Notification, error) {
	return activeProvider.fetchNotifications(ctx, db, limit, priority)
}

func DbSetStatus(ctx context.Context, db *sql.DB, id int64, status string) {
	activeProvider.setStatus(ctx, db, id, status)
}

func DbSetErrorStatusAndAdd1RetryCount(ctx context.Context, db *sql.DB, id int64) {
	activeProvider.setErrorStatusAndAdd1RetryCount(ctx, db, id)
}

func DbReapExpired(ctx context.Context, db *sql.DB) error {
	return activeProvider.reapExpired(ctx, db)
}

func DbScheduleRetry(ctx context.Context, db *sql.DB, id int64, backoffSec int) {
	activeProvider.scheduleRetry(ctx, db, id, backoffSec)
}

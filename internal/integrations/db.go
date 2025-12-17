package integrations

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"go-notifications-worker/internal/config"
	"go-notifications-worker/internal/constants"
	"go-notifications-worker/internal/models"
)

func InitDB(ctx context.Context) *sql.DB {
	// Connect SQL Server
	db, err := sql.Open("sqlserver", config.SqlConnString)
	if err != nil {
		log.Fatal("DB ERR:", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(config.DbMaxOpenConns)
	db.SetMaxIdleConns(config.DbMaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(config.DbConnMaxLifetimeMinutes) * time.Minute)
	db.SetConnMaxIdleTime(time.Duration(config.DbConnMaxIdleTimeMinutes) * time.Minute)

	// simple connection check
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}

	log.Println("DB connected successfully")
	return db
}

func DbFetchNotifications(ctx context.Context, db *sql.DB, limit int, priority int) ([]models.Notification, error) {
	// Start an explicit transaction
	tx, err := db.BeginTx(ctx, nil)
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

	rows, err := tx.QueryContext(ctx, query, limit, priority)
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

		_, err = tx.ExecContext(ctx, updateQuery)
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

func DbSetStatus(ctx context.Context, db *sql.DB, id int64, status string) {
	_, err := db.ExecContext(ctx, `
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

func DbSetErrorStatusAndAdd1RetryCount(ctx context.Context, db *sql.DB, id int64) {
	_, err := db.ExecContext(ctx, `
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

// reapExpired resets rows to be processed again
func DbReapExpired(ctx context.Context, db *sql.DB) error {
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

func DbScheduleRetry(ctx context.Context, db *sql.DB, id int64, backoffSec int) {
	_, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal 
		SET Status = 'pending', 
			RetryCount = RetryCount + 1,
			NextAttemptAt = DATEADD(SECOND, @p1, GETUTCDATE()),
			UpdatedAt=GETUTCDATE() 
			WHERE Id=@p2 
				AND (Status = 'processing' OR Status = 'pending')`,
		backoffSec, id)

	if err != nil {
		log.Printf("schedule retry err id=%d: %v", id, err)
	}
}

package integrations

import (
	"context"
	"database/sql"
	"log"

	"go-notifications-worker/internal/constants"
	"go-notifications-worker/internal/models"
)

type postgresProvider struct{}

func (p *postgresProvider) driverName() string {
	return "pgx"
}

// fetchNotifications uses a single atomic CTE that selects and marks rows as
// 'processing' in one round-trip, leveraging FOR UPDATE SKIP LOCKED so
// competing workers never block each other.
func (p *postgresProvider) fetchNotifications(ctx context.Context, db *sql.DB, limit int, priority int) ([]models.Notification, error) {
	query := `
        WITH cte AS (
            SELECT Id
            FROM NotificationJournal
            WHERE Status = 'pending'
              AND Priority = $2
              AND (NextAttemptAt IS NULL OR NextAttemptAt <= (NOW() AT TIME ZONE 'UTC'))
            ORDER BY CreatedAt
            LIMIT $1
            FOR UPDATE SKIP LOCKED
        )
        UPDATE NotificationJournal
        SET Status    = 'processing',
            UpdatedAt = (NOW() AT TIME ZONE 'UTC')
        FROM cte
        WHERE NotificationJournal.Id = cte.Id
        RETURNING NotificationJournal.Id, Type, Priority, "To", Subject, Body, RetryCount, MaxRetries
    `

	rows, err := db.QueryContext(ctx, query, limit, priority)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.Notification
	for rows.Next() {
		var n models.Notification
		if err := rows.Scan(&n.Id, &n.Type, &n.Priority, &n.To, &n.Subject, &n.Body, &n.RetryCount, &n.MaxRetries); err != nil {
			return nil, err
		}
		list = append(list, n)
	}
	return list, rows.Err()
}

func (p *postgresProvider) setStatus(ctx context.Context, db *sql.DB, id int64, status string) {
	_, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status    = $1,
		    UpdatedAt = (NOW() AT TIME ZONE 'UTC')
		WHERE Id = $2
		  AND Status != 'sent'`,
		status, id)
	if err != nil {
		log.Printf("set status err id=%d: %v", id, err)
	}
}

func (p *postgresProvider) setErrorStatusAndAdd1RetryCount(ctx context.Context, db *sql.DB, id int64) {
	_, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status     = $1,
		    RetryCount = RetryCount + 1,
		    UpdatedAt  = (NOW() AT TIME ZONE 'UTC')
		WHERE Id = $2
		  AND Status != 'sent'`,
		constants.NotificationStateError, id)
	if err != nil {
		log.Printf("set error status err id=%d: %v", id, err)
	}
}

func (p *postgresProvider) reapExpired(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status    = 'pending',
		    UpdatedAt = (NOW() AT TIME ZONE 'UTC')
		WHERE Status = 'processing'
		  AND UpdatedAt <= (NOW() AT TIME ZONE 'UTC') - ($1 * INTERVAL '1 second')`,
		activeReclaimBackoffSeconds())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("reaper: reset %d (to pending)\n", n)
	}
	return nil
}

func (p *postgresProvider) scheduleRetry(ctx context.Context, db *sql.DB, id int64, backoffSec int) {
	_, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status        = 'pending',
		    RetryCount    = RetryCount + 1,
		    NextAttemptAt = (NOW() AT TIME ZONE 'UTC') + ($1 * INTERVAL '1 second'),
		    UpdatedAt     = (NOW() AT TIME ZONE 'UTC')
		WHERE Id = $2
		  AND (Status = 'processing' OR Status = 'pending')`,
		backoffSec, id)
	if err != nil {
		log.Printf("schedule retry err id=%d: %v", id, err)
	}
}

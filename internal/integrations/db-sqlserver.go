package integrations

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"go-notifications-worker/internal/constants"
	"go-notifications-worker/internal/models"
)

/*
Troubleshoot SQL Server Connection Issue:

	if you get "DB ping failed: no instance matching '_SQLSERVERDEVInstanceName_' returned from host '_HOST_'", try the below:
		- Check SQL Server Browser Service is running
		- Check if TCP/IP is Enabled
			Open SQL Server Configuration Manager
			Go to SQL Server Network Configuration → Protocols for SQLSERVERDEV2025
			Enable TCP/IP
			Restart the SQL Server service
*/
type sqlServerProvider struct{}

func (p *sqlServerProvider) driverName() string {
	return "sqlserver"
}

func (p *sqlServerProvider) fetchNotifications(ctx context.Context, db *sql.DB, limit int, priority int) ([]models.Notification, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

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
		if err := rows.Scan(&n.Id, &n.Type, &n.Priority, &n.To, &n.Subject, &n.Body, &n.RetryCount, &n.MaxRetries); err != nil {
			return nil, err
		}
		list = append(list, n)
		ids = append(ids, n.Id)
	}
	rows.Close() //nolint:errcheck // intentional early close before update

	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			placeholders[i] = fmt.Sprintf("@p%d", i+1)
			args[i] = id
		}
		updateQuery := "UPDATE NotificationJournal SET Status = 'processing', UpdatedAt = GETUTCDATE() WHERE Id IN (" + strings.Join(placeholders, ",") + ")"
		if _, err = tx.ExecContext(ctx, updateQuery, args...); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return list, nil
}

func (p *sqlServerProvider) setStatus(ctx context.Context, db *sql.DB, id int64, status string) {
	_, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status = @p1,
		    UpdatedAt = GETUTCDATE()
		WHERE Id = @p2
		  AND Status != 'sent'`,
		status, id)
	if err != nil {
		log.Printf("set status err id=%d: %v", id, err)
	}
}

func (p *sqlServerProvider) setErrorStatusAndAdd1RetryCount(ctx context.Context, db *sql.DB, id int64) {
	_, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status = @p1,
		    RetryCount = RetryCount + 1,
		    UpdatedAt = GETUTCDATE()
		WHERE Id = @p2
		  AND Status != 'sent'`,
		constants.NotificationStateError, id)
	if err != nil {
		log.Printf("set error status err id=%d: %v", id, err)
	}
}

func (p *sqlServerProvider) reapExpired(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status = 'pending',
		    UpdatedAt = GETUTCDATE()
		WHERE Status = 'processing'
		  AND UpdatedAt <= DATEADD(SECOND, -@backoff, GETUTCDATE())
	`, sql.Named("backoff", activeReclaimBackoffSeconds()))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("reaper: reset %d (to pending)\n", n)
	}
	return nil
}

func (p *sqlServerProvider) scheduleRetry(ctx context.Context, db *sql.DB, id int64, backoffSec int) {
	_, err := db.ExecContext(ctx, `
		UPDATE NotificationJournal
		SET Status = 'pending',
		    RetryCount = RetryCount + 1,
		    NextAttemptAt = DATEADD(SECOND, @p1, GETUTCDATE()),
		    UpdatedAt = GETUTCDATE()
		WHERE Id = @p2
		  AND (Status = 'processing' OR Status = 'pending')`,
		backoffSec, id)
	if err != nil {
		log.Printf("schedule retry err id=%d: %v", id, err)
	}
}

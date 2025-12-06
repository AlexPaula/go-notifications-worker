package connections

import (
	"context"
	"database/sql"
	"log"

	"go-notifications-worker/internal/config"
)

func InitDB(ctx context.Context) *sql.DB {
	// Connect SQL Server
	db, err := sql.Open("sqlserver", config.SqlConnString)
	if err != nil {
		log.Fatal("DB ERR:", err)
	}
	log.Println("DB connected")

	// simple connection check
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	return db
}

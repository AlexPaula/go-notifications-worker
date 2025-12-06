package models

import "database/sql"

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

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

// PushMessage wraps a push notification with its metadata
type PushMessage struct {
	NotificationId int64
	Token          string
	Body           string
	Title          string
	RetryCount     int
	MaxRetries     int
	Priority       int // 1=high, 2=normal
}

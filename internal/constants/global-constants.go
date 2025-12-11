package constants

const (
	PriorityHigh   = 1
	PriorityNormal = 2

	NotificationTypeEmail = "email"
	NotificationTypePush  = "push"

	NotificationStatePending    = "pending"
	NotificationStateProcessing = "processing"
	NotificationStateSent       = "sent"
	NotificationStateError      = "error"

	FCMError_InvalidToken    = "fcm-invalid-token"
	FCMError_InvalidArgument = "fcm-invalid-argument"
	FCMError_Retry           = "fcm-retry"
)

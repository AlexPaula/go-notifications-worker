package connections

import (
	"context"
	"log"

	firebase "firebase.google.com/go"
	"firebase.google.com/go/messaging"
	"google.golang.org/api/option"

	"go-notifications-worker/internal/config"
)

func InitFirebase(ctx context.Context) *messaging.Client {
	// Connect Firebase
	app, err := firebase.NewApp(ctx, nil, option.WithCredentialsFile(config.FirebaseCredentialsFile))
	if err != nil {
		log.Fatal("FIREBASE ERR:", err)
	}
	fcmClient, err := app.Messaging(ctx)
	if err != nil {
		log.Fatal("FCM ERR:", err)
	}
	log.Println("Firebase connected")
	return fcmClient
}

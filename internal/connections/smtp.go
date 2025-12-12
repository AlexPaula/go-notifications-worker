package connections

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/smtp"
	"sync"
	"time"

	"go-notifications-worker/internal/config"
)

// SMTPConnectionWrapper holds the connection and metadata
type SMTPConnectionWrapper struct {
	Client    *smtp.Client
	CreatedAt time.Time
}

var (
	smtpPool        chan *SMTPConnectionWrapper
	poolMutex       sync.Mutex
	shutdownOnce    sync.Once
	shutdownCtx     context.Context
	shutdownCancel  context.CancelFunc
	healthCheckDone chan struct{} // Signal when health check is done

	// Connection max age before refresh (e.g., 5 minutes)
	maxConnectionAge = 5 * time.Minute
)

func InitSMTP() error {
	poolMutex.Lock()
	defer poolMutex.Unlock()

	// Create shutdown context for graceful shutdown
	shutdownCtx, shutdownCancel = context.WithCancel(context.Background())
	healthCheckDone = make(chan struct{})

	smtpPool = make(chan *SMTPConnectionWrapper, config.SmtpPoolSize)

	for i := 0; i < config.SmtpPoolSize; i++ {
		client, err := createSMTPConnection()
		if err != nil {
			return fmt.Errorf("SMTP Pool Init ERR: %w", err)
		}
		wrapper := &SMTPConnectionWrapper{
			Client:    client,
			CreatedAt: time.Now(),
		}
		smtpPool <- wrapper
	}

	// Start a background goroutine to periodically validate and refresh connections
	go healthCheckLoop(shutdownCtx)

	log.Printf("SMTP pool initialized with %d connections\n", config.SmtpPoolSize)
	return nil
}

func createSMTPConnection() (*smtp.Client, error) {
	client, err := smtp.Dial(config.SmtpHost + ":" + config.SmtpPort)
	if err != nil {
		return nil, err
	}

	if err := client.StartTLS(&tls.Config{ServerName: config.SmtpHost}); err != nil {
		client.Close()
		return nil, err
	}

	auth := smtp.PlainAuth("", config.SmtpFrom, config.SmtpPassword, config.SmtpHost)
	if err := client.Auth(auth); err != nil {
		client.Close()
		return nil, err
	}

	return client, nil
}

// GetSMTPConnection with timeout to prevent indefinite blocking
func GetSMTPConnection(ctx context.Context) (*smtp.Client, error) {
	// Try to get from pool with short timeout per attempt
	for attempt := 0; attempt < 3; attempt++ {
		// Create a short timeout for each attempt (2 seconds)
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)

		select {
		case wrapper := <-smtpPool:
			cancel()
			if wrapper == nil {
				return nil, fmt.Errorf("SMTP pool closed")
			}

			// Check if connection has exceeded max age
			if time.Since(wrapper.CreatedAt) > maxConnectionAge {
				log.Printf("SMTP connection exceeded max age (%v), refreshing", maxConnectionAge)
				wrapper.Client.Close()
				// Create a fresh connection
				newClient, err := createSMTPConnection()
				if err != nil {
					log.Printf("Failed to recreate aged SMTP connection: %v", err)
					continue // Try to get another connection from pool
				}
				newWrapper := &SMTPConnectionWrapper{
					Client:    newClient,
					CreatedAt: time.Now(),
				}
				return newWrapper.Client, nil
			}

			// Verify connection is alive before returning
			if err := wrapper.Client.Noop(); err != nil {
				log.Printf("SMTP connection noop failed (attempt %d/3): %v", attempt+1, err)
				wrapper.Client.Close()
				// Try to get another connection from pool
				continue
			}
			return wrapper.Client, nil

		case <-attemptCtx.Done():
			cancel()
			// Timeout waiting for a connection from pool, try again
			log.Printf("SMTP pool timeout (attempt %d/3), trying fresh connection or next attempt", attempt+1)
			continue

		case <-ctx.Done():
			cancel()
			return nil, ctx.Err()
		}
	}

	// All pool attempts failed, create a brand new direct connection
	log.Printf("All SMTP pool attempts failed, creating fresh connection")
	newClient, err := createSMTPConnection()
	if err != nil {
		return nil, fmt.Errorf("failed to create fresh SMTP connection: %w", err)
	}

	return newClient, nil
}

func ReturnSMTPConnection(client *smtp.Client, isHealthy bool) {
	if client == nil {
		return
	}

	if !isHealthy {
		log.Printf("Returning unhealthy SMTP connection, discarding")
		client.Close()
		// Create a new connection to replace the dead one
		newClient, err := createSMTPConnection()
		if err != nil {
			log.Printf("Failed to recreate SMTP connection after error: %v", err)
			// Don't put anything back - let the next request handle it
			return
		}
		wrapper := &SMTPConnectionWrapper{
			Client:    newClient,
			CreatedAt: time.Now(),
		}
		// Non-blocking send to pool
		select {
		case smtpPool <- wrapper:
		default:
			log.Println("SMTP pool full, discarding fresh connection")
			wrapper.Client.Close()
		}
		return
	}

	// Connection is healthy, return it to the pool
	wrapper := &SMTPConnectionWrapper{
		Client:    client,
		CreatedAt: time.Now(), // Reset age timer
	}

	// Non-blocking send to pool (discard if pool is full, which shouldn't happen)
	select {
	case smtpPool <- wrapper:
	default:
		log.Println("SMTP pool full, discarding connection")
		wrapper.Client.Close()
	}
}

// ClosePool gracefully shuts down the SMTP pool
func ClosePool() {
	shutdownOnce.Do(func() {
		// Signal health check loop to stop
		if shutdownCancel != nil {
			shutdownCancel()
		}

		// Wait for health check goroutine to finish (prevents concurrent access)
		if healthCheckDone != nil {
			<-healthCheckDone
		}

		poolMutex.Lock()
		defer poolMutex.Unlock()

		if smtpPool == nil {
			return
		}

		// Drain and close all connections
		close(smtpPool)
		for wrapper := range smtpPool {
			if wrapper != nil && wrapper.Client != nil {
				wrapper.Client.Close()
			}
		}
		log.Println("SMTP pool closed")
	})
}

// healthCheckLoop periodically validates and refreshes connections in the pool
func healthCheckLoop(ctx context.Context) {
	defer close(healthCheckDone) // Signal that we're done

	ticker := time.NewTicker(15 * time.Second) // Check every 15 seconds (more aggressive)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("SMTP health check loop shutting down")
			return
		case <-ticker.C:
			// Periodically validate pool health
			validatePoolHealth()
		}
	}
}

// validatePoolHealth checks connections without blocking the main pool
func validatePoolHealth() {
	poolSize := len(smtpPool)

	// Sample all connections from the pool (non-blocking) or at least half
	checkLimit := poolSize
	if checkLimit > 10 {
		checkLimit = 10
	}

	for i := 0; i < checkLimit; i++ {
		select {
		case wrapper := <-smtpPool:
			if wrapper == nil {
				continue
			}

			// Quick noop check
			isValid := wrapper.Client.Noop() == nil

			// Check age
			age := time.Since(wrapper.CreatedAt)
			isFresh := age < maxConnectionAge

			if !isValid || !isFresh {
				logMsg := "invalid"
				if !isFresh {
					logMsg = "aged"
				}
				log.Printf("SMTP health check: connection %s (valid=%v, age=%v) - replacing", logMsg, isValid, age)
				wrapper.Client.Close()

				// Create replacement
				newClient, err := createSMTPConnection()
				if err != nil {
					log.Printf("Failed to create replacement SMTP connection in health check: %v", err)
					continue
				}
				newWrapper := &SMTPConnectionWrapper{
					Client:    newClient,
					CreatedAt: time.Now(),
				}
				// Put it back in the pool
				select {
				case smtpPool <- newWrapper:
				default:
					log.Println("SMTP pool full during health check, discarding replacement")
					newWrapper.Client.Close()
				}
			} else {
				// Connection is good, return it to pool
				select {
				case smtpPool <- wrapper:
				default:
					log.Println("SMTP pool full during health check, discarding connection")
					wrapper.Client.Close()
				}
			}
		default:
			// Pool is empty or mostly empty, don't block
			return
		}
	}
}

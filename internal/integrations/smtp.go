package integrations

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/smtp"
	"regexp"
	"strings"
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
	smtpPool     chan *SMTPConnectionWrapper
	poolMutex    sync.Mutex
	shutdownOnce sync.Once
	// shutdownCtx     context.Context // Passed from main
	healthCheckDone chan struct{} // Signal when health check is done
	targetPoolSize  int           // Target number of connections to maintain
	currentPoolSize int           // Current actual connections in existence
	sizeMutex       sync.Mutex    // Protects currentPoolSize counter

	// Connection max age before refresh (e.g., 5 minutes)
	maxConnectionAge = 5 * time.Minute
)

func InitSMTP(ctx context.Context) error {
	poolMutex.Lock()
	defer poolMutex.Unlock()

	// Store main context for graceful shutdown
	// shutdownCtx = ctx
	healthCheckDone = make(chan struct{})

	targetPoolSize = config.SmtpPoolSize
	currentPoolSize = 0
	smtpPool = make(chan *SMTPConnectionWrapper, config.SmtpPoolSize)

	for i := 0; i < config.SmtpPoolSize; i++ {
		client, err := SMTPCreateConnection(ctx)
		if err != nil {
			return fmt.Errorf("SMTP Pool Init ERR: %w", err)
		}
		wrapper := &SMTPConnectionWrapper{
			Client:    client,
			CreatedAt: time.Now(),
		}
		smtpPool <- wrapper
		sizeMutex.Lock()
		currentPoolSize++
		sizeMutex.Unlock()
	}

	// Start a background goroutine to periodically validate and refresh connections
	go healthCheckLoop(ctx)

	log.Printf("SMTP pool initialized with %d connections\n", config.SmtpPoolSize)
	return nil
}

func SMTPCreateConnection(ctx context.Context) (*smtp.Client, error) {
	// Check if context is already cancelled
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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

// SMTPGetConnection with timeout to prevent indefinite blocking
func SMTPGetConnection(ctx context.Context) (*smtp.Client, error) {
	// Try to get from pool with short timeout per attempt
	for attempt := 0; attempt < 3; attempt++ {
		// Use non-blocking select first to check if pool has connections
		select {
		case wrapper := <-smtpPool:
			if wrapper == nil {
				return nil, fmt.Errorf("SMTP pool closed")
			}

			// Check if connection has exceeded max age
			if time.Since(wrapper.CreatedAt) > maxConnectionAge {
				log.Printf("SMTP connection exceeded max age (%v), refreshing", maxConnectionAge)
				wrapper.Client.Close()
				// Create a fresh connection
				newClient, err := SMTPCreateConnection(ctx)
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

		default:
			// Pool is empty, don't wait - create direct connection immediately
			if attempt == 0 {
				log.Printf("SMTP pool empty, creating direct connection")
			}
		}

		// Check if context is cancelled before creating connection
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Pool is empty or previous attempts failed, create a brand new direct connection
		newClient, err := SMTPCreateConnection(ctx)
		if err != nil {
			log.Printf("Failed to create direct SMTP connection (attempt %d/3): %v", attempt+1, err)
			// Brief backoff before retry
			time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
			continue
		}

		log.Printf("Created direct SMTP connection (pool was empty or unavailable)")
		return newClient, nil
	}

	return nil, fmt.Errorf("failed to get SMTP connection after 3 attempts")
}

func SMTPReturnConnection(ctx context.Context, client *smtp.Client, isHealthy bool) {
	if client == nil {
		return
	}

	if !isHealthy {
		log.Printf("Returning unhealthy SMTP connection, discarding")
		client.Close()
		sizeMutex.Lock()
		currentPoolSize--
		sizeMutex.Unlock()

		// Trigger async replenishment only if not shutting down
		select {
		case <-ctx.Done():
			log.Println("SMTP pool shutdown in progress, skipping replenishment")
		default:
			// Trigger async replenishment - don't block the worker
			go replenishPool(ctx)
		}
		return
	}

	// Connection is healthy, return it to the pool
	wrapper := &SMTPConnectionWrapper{
		Client:    client,
		CreatedAt: time.Now(), // Reset age timer
	}

	// Non-blocking send to pool (discard if pool is full or shutdown in progress)
	select {
	case smtpPool <- wrapper:
	case <-ctx.Done():
		log.Println("SMTP pool shutdown in progress, closing returned connection")
		wrapper.Client.Close()
	default:
		log.Println("SMTP pool full, discarding connection")
		wrapper.Client.Close()
	}
}

// SMTPClosePool gracefully shuts down the SMTP pool
func SMTPClosePool() {
	shutdownOnce.Do(func() {
		// Wait for health check goroutine to finish (prevents concurrent access)
		// The health check will stop automatically when shutdownCtx is cancelled
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
			validatePoolHealth(ctx)
		}
	}
}

// validatePoolHealth checks connections without blocking the main pool
func validatePoolHealth(ctx context.Context) {
	poolSize := len(smtpPool)

	// Sample all connections from the pool (non-blocking) or at least half
	checkLimit := poolSize
	if checkLimit > 10 {
		checkLimit = 10
	}

	for i := 0; i < checkLimit; i++ {
		select {
		case <-ctx.Done():
			log.Println("SMTP validate pool health loop shutting down")
			return
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
				sizeMutex.Lock()
				currentPoolSize--
				sizeMutex.Unlock()

				// Create replacement
				newClient, err := SMTPCreateConnection(ctx)
				if err != nil {
					log.Printf("Failed to create replacement SMTP connection in health check: %v, will retry", err)
					continue
				}
				newWrapper := &SMTPConnectionWrapper{
					Client:    newClient,
					CreatedAt: time.Now(),
				}
				// Put it back in the pool
				select {
				case smtpPool <- newWrapper:
					sizeMutex.Lock()
					currentPoolSize++
					sizeMutex.Unlock()
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

	// After health check, ensure pool is at target size
	replenishPool(ctx)
}

// replenishPool asynchronously creates connections to reach target pool size
func replenishPool(ctx context.Context) {
	// Check if shutdown is in progress
	select {
	case <-ctx.Done():
		log.Println("SMTP pool replenishment: shutdown in progress, skipping")
		return
	default:
	}

	sizeMutex.Lock()
	current := currentPoolSize
	target := targetPoolSize
	sizeMutex.Unlock()

	if current >= target {
		return
	}

	needed := target - current
	log.Printf("SMTP pool replenishment: current=%d, target=%d, creating %d connections", current, target, needed)

	for i := 0; i < needed; i++ {
		// Check shutdown before each connection attempt
		select {
		case <-ctx.Done():
			log.Printf("SMTP pool replenishment: shutdown detected, stopping at %d/%d", i, needed)
			return
		default:
		}

		newClient, err := SMTPCreateConnection(ctx)
		if err != nil {
			log.Printf("Failed to create SMTP connection during replenishment (attempt %d/%d): %v", i+1, needed, err)
			continue
		}

		newWrapper := &SMTPConnectionWrapper{
			Client:    newClient,
			CreatedAt: time.Now(),
		}

		// Non-blocking send to pool with shutdown check
		select {
		case smtpPool <- newWrapper:
			sizeMutex.Lock()
			currentPoolSize++
			sizeMutex.Unlock()
			log.Printf("SMTP pool replenishment: added connection (%d/%d)", i+1, needed)
		case <-ctx.Done():
			// Shutdown started, close the connection we just created
			newWrapper.Client.Close()
			log.Printf("SMTP pool replenishment: shutdown during send, stopping at %d/%d", i, needed)
			return
		default:
			// Pool is full, connection not needed
			newWrapper.Client.Close()
			log.Printf("SMTP pool replenishment: pool full, stopping early at %d/%d", i, needed)
			return
		}
	}
}

// stripHTMLTags removes HTML tags from a string, returning plain text
func stripHTMLTags(html string) string {
	// Remove script and style elements
	re := regexp.MustCompile(`(?s)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>`)
	text := re.ReplaceAllString(html, "")

	// Remove all HTML tags
	re = regexp.MustCompile(`<[^>]*>`)
	text = re.ReplaceAllString(text, "")

	// Decode HTML entities and collapse whitespace
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)

	return text
}

func SendEmail(ctx context.Context, to, cc, bcc, subject, body string, isBodyHtml bool) error {
	// Get connection with context timeout (fails fast if no connections available)
	client, err := SMTPGetConnection(ctx)
	if err != nil {
		return fmt.Errorf("failed to get SMTP connection: %w", err)
	}

	isHealthy := true
	defer func() {
		SMTPReturnConnection(ctx, client, isHealthy)
	}()

	// Build message headers (CC visible, BCC NOT included)
	msg := "From: " + config.SmtpFrom + "\r\n" +
		"To: " + to + "\r\n"

	if cc != "" {
		msg += "Cc: " + cc + "\r\n"
	}
	// NOTE: Do NOT add Bcc to headers - it's hidden!

	msg += "Subject: " + subject + "\r\n"

	// Build message body based on content type
	if isBodyHtml {
		// Use MIME multipart for HTML email with plain text fallback
		boundary := "===============" + fmt.Sprintf("%d", time.Now().UnixNano()) + "==============="
		msg += "MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n" +
			"\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: 7bit\r\n" +
			"\r\n" +
			stripHTMLTags(body) + "\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: 7bit\r\n" +
			"\r\n" +
			body + "\r\n" +
			"--" + boundary + "--\r\n"
	} else {
		// Simple plain text email
		msg += "Content-Type: text/plain; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: 7bit\r\n" +
			"\r\n" +
			body + "\r\n"
	}

	// Build SMTP recipients list (includes To, Cc, AND Bcc)
	recipients := []string{to}
	if cc != "" {
		recipients = append(recipients, strings.Split(cc, ",")...)
	}
	if bcc != "" {
		recipients = append(recipients, strings.Split(bcc, ",")...)
	}

	// Clean up whitespace in recipient emails
	for i := range recipients {
		recipients[i] = strings.TrimSpace(recipients[i])
	}

	// Send mail

	/*
	   The SMTP sequence is:

	   Mail()  - Tell the server who the email is FROM
	   Rcpt()  - Tell the server who to send TO (called once per recipient)
	   Data()  - Send the actual message content (Opens a write channel to send the message content)
	   Write() - Writes the actual email message (headers + body) to that channel

	   So the Rcpt() loop tells the server all the addresses that should receive this email (To + Cc + Bcc),
	   and then Data() sends the actual message body. The server handles delivering to all those recipients.
	*/

	if err := client.Mail(config.SmtpFrom); err != nil {
		isHealthy = false
		return err
	}

	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			isHealthy = false
			return err
		}
	}

	wc, err := client.Data()
	if err != nil {
		isHealthy = false
		return err
	}
	defer wc.Close()

	_, err = wc.Write([]byte(msg))
	if err != nil {
		isHealthy = false
	}
	return err
}

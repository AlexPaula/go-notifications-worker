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

	var client *smtp.Client
	var err error

	// Configure TLS settings
	tlsConfig := &tls.Config{
		ServerName: config.SmtpHost,
		MinVersion: tls.VersionTLS12, // Enforce minimum TLS 1.2
	}

	// Connect based on TLS mode
	switch config.SmtpTLSMode {
	case "tls":
		// Implicit TLS (SMTPS) - encrypted from the start (port 465 typically)
		conn, err := tls.Dial("tcp", config.SmtpHost+":"+config.SmtpPort, tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to establish TLS connection: %w", err)
		}

		client, err = smtp.NewClient(conn, config.SmtpHost)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to create SMTP client: %w", err)
		}

	case "starttls":
		// STARTTLS - start plain then upgrade to TLS (port 587 typically)
		client, err = smtp.Dial(config.SmtpHost + ":" + config.SmtpPort)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to SMTP server: %w", err)
		}

		if err := client.StartTLS(tlsConfig); err != nil {
			client.Close()
			return nil, fmt.Errorf("failed to start TLS: %w", err)
		}

	case "none":
		// No TLS - insecure, only for local development
		log.Println("WARNING: SMTP TLS disabled - connection is insecure!")
		client, err = smtp.Dial(config.SmtpHost + ":" + config.SmtpPort)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to SMTP server: %w", err)
		}

	default:
		return nil, fmt.Errorf("invalid SMTP_TLS_MODE: %s (must be 'tls', 'starttls', or 'none')", config.SmtpTLSMode)
	}

	// Authenticate (only if password is provided)
	// For port 25 relay servers, authentication may not be required
	if config.SmtpPassword != "" {
		auth := smtp.PlainAuth("", config.SmtpUsername, config.SmtpPassword, config.SmtpHost)
		if err := client.Auth(auth); err != nil {
			client.Close()
			return nil, fmt.Errorf("SMTP authentication failed: %w", err)
		}
	} else {
		if config.SmtpTLSMode == "none" {
			log.Println("SMTP: No password provided, skipping authentication (insecure relay mode)")
		} else {
			log.Printf("SMTP: No password provided with %s mode - ensure server allows unauthenticated connections", config.SmtpTLSMode)
		}
	}

	return client, nil
}

func SMTPGetConnection(ctx context.Context) (*smtp.Client, error) {
	// Retry loop with total timeout
	deadline := time.Now().Add(30 * time.Second)
	attempt := 0

	for time.Now().Before(deadline) {
		attempt++

		// Try to get a connection from the pool
		select {
		case wrapper := <-smtpPool:
			if wrapper == nil {
				return nil, fmt.Errorf("SMTP pool closed")
			}

			// Check if connection has exceeded max age
			if time.Since(wrapper.CreatedAt) > maxConnectionAge {
				log.Printf("SMTP connection exceeded max age (%v), refreshing", maxConnectionAge)
				wrapper.Client.Close()
				sizeMutex.Lock()
				currentPoolSize--
				sizeMutex.Unlock()

				// Create a fresh connection and return it
				newClient, err := SMTPCreateConnection(ctx)
				if err != nil {
					log.Printf("Failed to recreate aged SMTP connection (attempt %d): %v", attempt, err)
					go replenishPool(ctx)
					time.Sleep(100 * time.Millisecond) // Brief pause before retry
					continue
				}
				sizeMutex.Lock()
				currentPoolSize++
				sizeMutex.Unlock()
				return newClient, nil
			}

			// Verify connection is alive before returning
			if err := wrapper.Client.Noop(); err != nil {
				log.Printf("SMTP connection noop failed (attempt %d): %v", attempt, err)
				wrapper.Client.Close()
				sizeMutex.Lock()
				currentPoolSize--
				sizeMutex.Unlock()
				go replenishPool(ctx)
				time.Sleep(100 * time.Millisecond) // Brief pause before retry
				continue
			}

			return wrapper.Client, nil

		case <-ctx.Done():
			return nil, ctx.Err()

		case <-time.After(200 * time.Millisecond):
			// No connection available right now, retry
			// This prevents tight loop spinning
			continue
		}
	}

	return nil, fmt.Errorf("timeout waiting for SMTP connection after %d attempts in 30s", attempt)
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
			go replenishPool(ctx)
		}
		return
	}

	// Connection is healthy, return it to the pool
	wrapper := &SMTPConnectionWrapper{
		Client:    client,
		CreatedAt: time.Now(), // Reset age timer
	}

	// Blocking send with timeout to ensure connection is returned
	select {
	case smtpPool <- wrapper:
		// Successfully returned to pool
	case <-time.After(5 * time.Second):
		// This shouldn't happen unless pool is stuck
		log.Println("SMTP pool return timeout after 5s, discarding connection")
		wrapper.Client.Close()
		sizeMutex.Lock()
		currentPoolSize--
		sizeMutex.Unlock()
	case <-ctx.Done():
		log.Println("SMTP pool shutdown in progress, closing returned connection")
		wrapper.Client.Close()
		sizeMutex.Lock()
		currentPoolSize--
		sizeMutex.Unlock()
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

	// Sample connections from the pool
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
				// Put it back in the pool with timeout (blocking)
				select {
				case smtpPool <- newWrapper:
					sizeMutex.Lock()
					currentPoolSize++
					sizeMutex.Unlock()
				case <-time.After(5 * time.Second):
					log.Println("SMTP pool return timeout during health check, discarding replacement")
					newWrapper.Client.Close()
				case <-ctx.Done():
					log.Println("SMTP health check: shutdown during return, closing connection")
					newWrapper.Client.Close()
					return
				}
			} else {
				// Connection is good, return it to pool with timeout
				select {
				case smtpPool <- wrapper:
				case <-time.After(5 * time.Second):
					log.Println("SMTP pool return timeout during health check, discarding connection")
					wrapper.Client.Close()
				case <-ctx.Done():
					log.Println("SMTP health check: shutdown during return, closing connection")
					wrapper.Client.Close()
					return
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

		// Blocking send to pool with timeout
		select {
		case smtpPool <- newWrapper:
			sizeMutex.Lock()
			currentPoolSize++
			sizeMutex.Unlock()
			log.Printf("SMTP pool replenishment: added connection (%d/%d)", i+1, needed)
		case <-time.After(5 * time.Second):
			// Pool stuck, discard
			newWrapper.Client.Close()
			log.Printf("SMTP pool replenishment: timeout during send, stopping at %d/%d", i, needed)
			return
		case <-ctx.Done():
			// Shutdown started, close the connection we just created
			newWrapper.Client.Close()
			log.Printf("SMTP pool replenishment: shutdown during send, stopping at %d/%d", i, needed)
			return
		}
	}
}

// isValidEmail performs basic email validation
func isValidEmail(email string) bool {
	email = strings.TrimSpace(email)
	if email == "" {
		return false
	}
	// Basic email validation regex
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)
	return emailRegex.MatchString(email)
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
	recipients := []string{}

	// Helper function to split by comma or semicolon
	splitEmails := func(emails string) []string {
		// Replace semicolons with commas for consistent splitting
		emails = strings.ReplaceAll(emails, ";", ",")
		return strings.Split(emails, ",")
	}

	// Add To recipients
	if to != "" {
		recipients = append(recipients, splitEmails(to)...)
	}

	// Add Cc recipients
	if cc != "" {
		recipients = append(recipients, splitEmails(cc)...)
	}

	// Add Bcc recipients
	if bcc != "" {
		recipients = append(recipients, splitEmails(bcc)...)
	}

	// Clean up whitespace and validate emails
	validRecipients := []string{}
	for _, recipient := range recipients {
		recipient = strings.TrimSpace(recipient)
		if recipient != "" {
			if isValidEmail(recipient) {
				validRecipients = append(validRecipients, recipient)
			} else {
				log.Printf("Warning: Invalid email address skipped: %s", recipient)
			}
		}
	}

	if len(validRecipients) == 0 {
		return fmt.Errorf("no valid recipient email addresses found")
	}

	recipients = validRecipients

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

# go-notifications-worker

Go (Golang) background service to send notifications.
- This is a **test** project and my first Go interaction. It needs a lot of improvements.

## Features

- **High-priority and normal-priority queues** with separate rate limiting
- **Email notifications** via SMTP
- **Push notifications** via Firebase Cloud Messaging (FCM)
- **Concurrent processing** with configurable worker pools
- **Graceful shutdown** handling
- **Metrics tracking** and periodic reporting
- **Health check endpoint** for monitoring and orchestration

## Setup

### Prerequisites

- Go 1.21 or higher
- SQL Server database
- Firebase Admin SDK credentials (for push notifications)
- SMTP server access (for email notifications)

### Installation

1. Clone the repository:
```bash
git clone <repository-url>
cd go-notifications-worker
```

2. Install dependencies:
```bash
go mod tidy
```

3. Configure environment variables (see Configuration section below)

4. Place your Firebase Admin SDK JSON file in the project root (or specify a different path in `.env`)

### Configuration

This application uses environment variables for configuration. Create a `.env` file in the project root:

```bash
cp .env.example .env
```

Then edit `.env` with your actual values:

#### Required Configuration

**Database Configuration:**
- **`DB_CONNECTION_STRING`**: SQL Server connection string
  ```
  DB_CONNECTION_STRING=Data Source=YOUR_SERVER;database=YOUR_DATABASE;Integrated Security=True;Persist Security Info=False;TrustServerCertificate=True;
  ```

**Email Configuration (SMTP):**
- **`SMTP_FROM`**: Email address to send from
- **`SMTP_PASSWORD`**: SMTP password
- **`SMTP_HOST`**: SMTP server hostname
- **`SMTP_PORT`**: SMTP server port (default: 25)

**Firebase Configuration:**
- **`FIREBASE_CREDENTIALS_FILE`**: Path to Firebase Admin SDK JSON file (default: `firebase-adminsdk.json`)

**Health Check Configuration:**
- **`HEALTH_CHECK_PORT`**: Port for the health check HTTP server (default: 8080)

**Worker Pool Configuration:**
- **`CLAIM_BATCH_HIGH`**: Batch size for high-priority notifications (default: 200)
- **`CLAIM_BATCH_NORMAL`**: Batch size for normal-priority notifications (default: 500)
- **`HIGH_PRIORITY_WORKER_POOL_SIZE`**: Number of concurrent workers for high-priority (default: 200)
- **`NORMAL_PRIORITY_WORKER_POOL_SIZE`**: Number of concurrent workers for normal-priority (default: 200)

**Rate Limiting Configuration:**
- **`HIGH_PRIORITY_RATE_LIMIT`**: Rate limit for high-priority notifications per second (default: 700)
- **`NORMAL_PRIORITY_RATE_LIMIT`**: Rate limit for normal-priority notifications per second (default: 300)

**Retry and Recovery Configuration:**
- **`REAPER_INTERVAL_SECONDS`**: Interval in seconds for the reaper loop to reclaim stuck notifications (default: 15)
- **`RECLAIM_BACKOFF_SECONDS`**: Seconds to wait before allowing a stuck notification to be reclaimed (default: 60)
- **`RETRY_HIGH_BACKOFF_SECONDS`**: Initial backoff in seconds for high-priority notification retries (default: 15)
- **`RETRY_NORMAL_BACKOFF_SECONDS`**: Initial backoff in seconds for normal-priority notification retries (default: 30)

### SQL Server Configuration

For the SQL Server connection to work, you need to enable the TCP/IP protocol in the SQL Server Configuration Manager.

View: https://stackoverflow.com/a/36482195

**To open SQL Server Configuration Manager:**
Press Windows + R keys together, then type `SQLServerManager16.msc` and press Enter.

### Running the Application

To run the notification worker:

```bash
go run cmd/worker/main.go
```

To build an executable:

```bash
go build -o notifications-worker.exe ./cmd/worker
```

Then run:

```bash
./notifications-worker.exe
```

### Graceful Shutdown

The worker handles `SIGINT` (Ctrl+C) and `SIGTERM` signals gracefully:
- Stops accepting new notifications
- Completes processing of in-flight notifications
- Logs final metrics
- Closes database and Firebase connections

## Database Schema

The application expects a `NotificationJournal` table with the following structure:

```sql
CREATE TABLE NotificationJournal (
    Id BIGINT PRIMARY KEY IDENTITY(1,1),
    Type VARCHAR(5) NOT NULL, -- 'email' or 'push'
    Priority TINYINT NOT NULL, -- 1 = high, 2 = normal
    [To] VARCHAR(500) NOT NULL,
    Subject NVARCHAR(500),
    Body NVARCHAR(MAX),
    Status VARCHAR(10) NOT NULL, -- 'pending', 'processing', 'sent', 'error'
    RetryCount TINYINT DEFAULT 0,
    MaxRetries TINYINT DEFAULT 5,
    CreatedAt DATETIME2 DEFAULT GETUTCDATE(),
    UpdatedAt DATETIME2 DEFAULT GETUTCDATE(),
    NextAttemptAt DATETIME2 NULL
);

CREATE INDEX IX_NotificationJournal_GetPending
ON NotificationJournal(Status, Priority, CreatedAt, NextAttemptAt)
INCLUDE (Type, [To], Subject, Body, RetryCount, MaxRetries)
WHERE Status = 'pending'
```

## Metrics

The worker logs metrics every 30 seconds, including:
- Total notifications processed
- Breakdown by priority (high/normal)
- Breakdown by type (email/push)
- Error counts
- Rate limiting statistics
- Average processing time
- Throughput (notifications per second)

## Health Check Endpoint

The worker exposes an HTTP health check endpoint for monitoring and orchestration purposes. This is particularly useful for:
- **Container orchestration** (Kubernetes, Docker Swarm) liveness and readiness probes
- **Load balancers** to verify instance health
- **Monitoring systems** to track worker status and metrics
- **CI/CD pipelines** to verify deployment success

### Available Endpoints

- **`GET /health`** - Returns health status and metrics
- **`GET /healthz`** - Alternative endpoint (Kubernetes convention)

### Usage

Check the worker's health status:

```bash
curl http://localhost:8080/health
```

### Response Format

```json
{
  "status": "healthy",
  "worker_id": "da44bc49-8c89-40a4-bc10-f1686357ad1f-36716",
  "uptime": "1h23m45s",
  "total_processed": 15234,
  "high_priority_processed": 8500,
  "normal_priority_processed": 6734,
  "emails_sent": 12000,
  "push_sent": 3234,
  "total_errors": 45
}
```

### Response Fields

- **`status`**: Current health status (always "healthy" when responding)
- **`worker_id`**: Unique identifier for this worker instance
- **`uptime`**: How long the worker has been running
- **`total_processed`**: Total number of notifications processed
- **`high_priority_processed`**: Number of high-priority notifications processed
- **`normal_priority_processed`**: Number of normal-priority notifications processed
- **`emails_sent`**: Number of emails successfully sent
- **`push_sent`**: Number of push notifications successfully sent
- **`total_errors`**: Total number of errors encountered

### Kubernetes Example

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: notifications-worker
spec:
  containers:
  - name: worker
    image: notifications-worker:latest
    ports:
    - containerPort: 8080
    livenessProbe:
      httpGet:
        path: /healthz
        port: 8080
      initialDelaySeconds: 30
      periodSeconds: 10
    readinessProbe:
      httpGet:
        path: /health
        port: 8080
      initialDelaySeconds: 5
      periodSeconds: 5
```

## Security

**Important:** Never commit sensitive credentials to version control!

- The `.env` file is gitignored and should contain your actual credentials
- The `.env.example` file is committed and serves as a template
- The `firebase-adminsdk.json` file is gitignored
- All sensitive configuration is loaded from environment variables

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
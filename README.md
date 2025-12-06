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

- **`DB_CONNECTION_STRING`**: SQL Server connection string
  ```
  DB_CONNECTION_STRING=Data Source=YOUR_SERVER;database=YOUR_DATABASE;Integrated Security=True;Persist Security Info=False;TrustServerCertificate=True;
  ```

- **`SMTP_FROM`**: Email address to send from
- **`SMTP_PASSWORD`**: SMTP password
- **`SMTP_HOST`**: SMTP server hostname
- **`SMTP_PORT`**: SMTP server port (default: 25)

- **`FIREBASE_CREDENTIALS_FILE`**: Path to Firebase Admin SDK JSON file (default: `firebase-adminsdk.json`)

- **`CLAIM_BATCH_HIGH`**: Batch size for high-priority notifications (default: 200)
- **`CLAIM_BATCH_NORMAL`**: Batch size for normal-priority notifications (default: 500)
- **`HIGH_PRIORITY_WORKER_POOL_SIZE`**: Number of concurrent workers for high-priority (default: 200)
- **`NORMAL_PRIORITY_WORKER_POOL_SIZE`**: Number of concurrent workers for normal-priority (default: 200)
- **`HIGH_PRIORITY_RATE_LIMIT`**: Rate limit for high-priority notifications per second (default: 700)
- **`NORMAL_PRIORITY_RATE_LIMIT`**: Rate limit for normal-priority notifications per second (default: 300)

### SQL Server Configuration

For the SQL Server connection to work, you need to enable the TCP/IP protocol in the SQL Server Configuration Manager.

View: https://stackoverflow.com/a/36482195

**To open SQL Server Configuration Manager:**
Press Windows + R keys together, then type `SQLServerManager16.msc` and press Enter.

### Running the Application

To run the notification worker:

```bash
go run worker.go
```

To build an executable:

```bash
go build -o notifications-worker.exe worker.go
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
    UpdatedAt DATETIME2 DEFAULT GETUTCDATE()
);

CREATE INDEX IX_NotificationJournal_Status_Priority 
ON NotificationJournal(Status, Priority, CreatedAt);
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

## Security

**Important:** Never commit sensitive credentials to version control!

- The `.env` file is gitignored and should contain your actual credentials
- The `.env.example` file is committed and serves as a template
- The `firebase-adminsdk.json` file is gitignored
- All sensitive configuration is loaded from environment variables

## TODO

- Implement notification retries with exponential backoff (add NextAttemptAt column)
- Implement Firebase backoff strategy for rate limiting
- Re-analyze and optimize metrics
- Organize code into separate files/packages
- Implement structured logging (e.g., with zerolog or zap)
- Add unit tests
- Add integration tests
- Add Docker support
- Add health check endpoint

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
# PayFlow — Payment Gateway API

PayFlow is a production-style payment gateway API built in Go that simulates how fintech systems process bank payments. It handles the full transaction lifecycle: accepting payments, communicating with bank clients, tracking state, and notifying merchants with the reliability and auditability that financial systems require.

Designed to demonstrate production-oriented backend architecture: clean layering, distributed systems patterns, and the operational rigor expected in regulated environments.

### Tech Highlights

- Hexagonal architecture (ports and adapters)
- Transactional outbox pattern (Kafka)
- Circuit breaker + retry with exponential backoff and jitter
- Idempotent payment processing (app + DB UNIQUE constraint)
- State machine with full audit trail
- Structured logging + p50/p95/p99 metrics
- Dockerized deployment

## Architecture

```text
                         ┌─────────────────────────┐
                         │      Load Balancer       │
                         └────────────┬────────────┘
                                      │
              ┌───────────────────────┬┴┬───────────────────────┐
              │                       │ │                       │
     ┌────────▼────────┐    ┌────────▼─▼──────┐    ┌──────────▼────────┐
     │   PayFlow API   │    │   PayFlow API   │    │   PayFlow API     │
     │   (stateless)   │    │   (stateless)   │    │   (stateless)     │
     └────────┬────────┘    └────────┬────────┘    └──────────┬────────┘
              │                      │                        │
              └──────────────────────┼────────────────────────┘
                                     │
                    ┌────────────────┬┴┬────────────────┐
                    │                │ │                │
           ┌───────▼──────┐  ┌──────▼─▼─────┐  ┌──────▼──────┐
           │  PostgreSQL   │  │    Kafka      │  │   Redis     │
           │  (primary)    │  │  (events)     │  │ (rate limit │
           │               │  │               │  │  / cache)*  │
           └──────────────┘  └──────┬────────┘  └─────────────┘
                                    │
                    ┌───────────────┬┴┬───────────────┐
                    │               │ │               │
             ┌──────▼─────┐ ┌──────▼─▼────┐ ┌───────▼──────┐
             │  Webhook    │ │   Audit     │ │  Analytics   │
             │  Service    │ │   Logger    │ │  Pipeline    │
             └────────────┘ └─────────────┘ └──────────────┘

* Redis, webhooks, and analytics are shown for production reference. Redis is not implemented in this repository. The code here implements the API, PostgreSQL persistence, and Kafka-backed outbox flow.
```

## Payment Flow

```text
1. Client sends POST /transactions with idempotency key
2. API checks idempotency → if duplicate, return cached result
3. Validate merchant (active?) and amount (positive? under limit?)
4. DB TRANSACTION: save payment + audit event + outbox event → COMMIT
5. Call bank client with timeout (circuit breaker protected, retry on transient failures)
6. Bank approves → status COMPLETED → credit merchant balance
7. Bank fails → status FAILED → no balance change
8. Outbox worker publishes event to Kafka (async)
9. Downstream services receive notification (webhooks, analytics)
```

## Design Decisions and Trade-offs

| Decision | Chose | Over | Why |
|----------|-------|------|-----|
| Database | PostgreSQL | MongoDB | ACID transactions required for financial data. Money needs consistency, not eventual consistency |
| ORM | `database/sql` | GORM | Full control over queries and transactions. ORMs hide SQL complexity that matters in payment systems |
| Framework | chi | gin/echo | Lightweight, stdlib-compatible. `net/http` signatures, no vendor lock-in |
| Architecture | Hexagonal | Layered MVC | Repository interfaces enable testing without DB. Swap PostgreSQL for another persistence layer without touching business logic |
| Bank call | Sync + outbox | Fully async | Client gets immediate response. Outbox preserves downstream delivery intent even if Kafka is temporarily unavailable |
| Testing | Mocks + integration tests | Testcontainers-only | Unit tests stay fast, and PostgreSQL integration tests run in CI |
| Rate limiting | In-memory token bucket | Redis-based | Simpler for single instance. Production would use Redis for global enforcement across pods |

## Core Features

**Transaction Processing**
- State machine enforcement: `PENDING → PROCESSING → COMPLETED/FAILED → REFUNDED`
- Single `transition()` method — every status change validated, persisted, and audited in one place
- Atomic operations: transaction + audit event + outbox event in one DB commit

**Reliability**
- Idempotency keys prevent duplicate charges (application check + DB UNIQUE constraint)
- Circuit breaker on bank API (`CLOSED → OPEN → HALF_OPEN`) prevents cascading failures
- Retry with exponential backoff + jitter avoids thundering herd on recovery
- Transactional outbox pattern with Kafka publishing support

**Security & Auth**
- API key authentication per merchant (Bearer token → merchant lookup → context)
- Token bucket rate limiting per IP
- Non-root Docker container

**Observability**
- Structured JSON logging (slog) with request ID correlation
- p50/p95/p99 latency histograms per endpoint
- Health check endpoint for K8s liveness/readiness probes
- Request count + active request gauges

## Tech Stack

Go 1.25 · chi · PostgreSQL 16 · Kafka · Docker · basic Kubernetes manifests

Application metrics exposed via `/metrics` endpoint

## Project Structure

```text
payflow/
├── .github/
│   └── workflows/
│       └── ci.yml                    ← CI pipeline
├── cmd/server/main.go                ← wires everything, graceful shutdown
├── internal/
│   ├── domain/
│   │   ├── models.go                 ← Transaction, Merchant, state machine
│   │   └── errors.go                 ← sentinel errors (ErrInvalidAmount, etc.)
│   ├── repository/
│   │   ├── interfaces.go             ← DBTX + repository contracts
│   │   ├── postgres/
│   │   │   ├── db.go                 ← connection pool + health check
│   │   │   ├── merchant.go           ← merchant queries
│   │   │   ├── transaction.go        ← tx queries + pagination
│   │   │   ├── events.go             ← audit event and outbox queries
│   │   │   └── postgres_integration_test.go
│   │   └── mock/
│   │       └── repos.go              ← in-memory implementations for testing
│   ├── service/
│   │   ├── payment.go                ← business logic, state machine
│   │   ├── bank.go                   ← bank client implementations
│   │   ├── payment_test.go           ← service tests
│   │   ├── resilience.go             ← circuit breaker, retry
│   │   ├── resilience_test.go        ← resilience tests
│   │   └── outbox_worker.go          ← Kafka publisher
│   ├── handler/
│   │   ├── handlers.go               ← HTTP handlers, error mapping
│   │   └── handler_test.go           ← endpoint tests with httptest
│   ├── middleware/
│   │   └── middleware.go             ← auth, logging, rate limit, recovery, CORS
│   ├── metrics/
│   │   └── metrics.go                ← counters, histograms, gauges
│   ├── config/
│   │   └── config.go                 ← env-based configuration
│   └── concurrency/
│       ├── patterns.go               ← worker pool, verification pipeline
│       └── patterns_test.go          ← concurrency tests with -race
├── migrations/                       ← 6 manual SQL migration files (idempotent, ordered)
├── Dockerfile                        ← multi-stage container build
├── docker-compose.yml                ← PostgreSQL + Kafka + Zookeeper
├── Makefile
└── go.mod
```

## API

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| `GET` | `/health` | No | Liveness + readiness (DB ping) |
| `GET` | `/metrics` | No | p50/p95/p99 latency, request counts |
| `POST` | `/api/v1/transactions` | Bearer | Create payment |
| `GET` | `/api/v1/transactions/{id}` | Bearer | Get transaction |
| `POST` | `/api/v1/transactions/{id}/refund` | Bearer | Refund (atomic reverse) |
| `GET` | `/api/v1/transactions/{id}/events` | Bearer | Audit trail |
| `GET` | `/api/v1/merchants/{id}/balance` | Bearer | Merchant balance |
| `GET` | `/api/v1/merchants/{id}/transactions` | Bearer | Paginated list |

### Example: Create a payment

The authenticated merchant comes from the Bearer API key, so the request body does not include `merchant_id`.

```bash
curl -X POST http://localhost:8080/api/v1/transactions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk_live_maple_001" \
  -H "X-Idempotency-Key: order_12345" \
  -d '{
    "amount_cents": 15000,
    "currency": "CAD"
  }'
```

```json
{
  "success": true,
  "data": {
    "id": "tx_a1b2c3d4",
    "merchant_id": "m_001",
    "amount_cents": 15000,
    "currency": "CAD",
    "status": "COMPLETED",
    "idempotency_key": "order_12345",
    "created_at": "2025-03-01T10:30:00Z"
  }
}
```

## Quick Start

```bash
# Clone and start
git clone https://github.com/aszender/payflow.git
cd payflow
docker-compose up -d --build

# Verify
curl http://localhost:8080/health | jq

# Create a payment
curl -X POST http://localhost:8080/api/v1/transactions \
  -H "Authorization: Bearer sk_live_maple_001" \
  -H "Content-Type: application/json" \
  -H "X-Idempotency-Key: order_12345" \
  -d '{"amount_cents":15000,"currency":"CAD"}'

# Check merchant balance
curl http://localhost:8080/api/v1/merchants/m_001/balance \
  -H "Authorization: Bearer sk_live_maple_001" | jq
```

## Testing

```bash
go test -race -cover ./...
go test -v ./internal/service/
go test -v ./internal/handler/
```

Most tests run without Docker or PostgreSQL. PostgreSQL integration tests run when `TEST_DATABASE_URL` is set and are also wired into CI. The test suite covers service flows, validation errors, idempotency, refunds, balance tracking, state transitions, circuit breaker behavior, handler behavior, and repository integration paths.

## CI

GitHub Actions runs the automated validation pipeline on every push and pull request:

- `go test ./...`
- `go test -race ./...`
- PostgreSQL-backed integration tests
- Docker image build verification

## Key Patterns Implemented

### DBTX Interface
Repositories accept an interface satisfied by both `*sql.DB` and `*sql.Tx`. The service
layer calls `repo.WithTx(tx)` to run multiple repositories inside one database transaction.
Creating a payment atomically writes the transaction record, audit event, and outbox event —
if any fails, all roll back.

### Transactional Outbox
Events are written to an outbox table in the same DB transaction as the payment. A background
worker polls unpublished events and publishes to Kafka. This demonstrates the transactional
outbox pattern and decouples event delivery from the request path.

### State Machine
Every status transition goes through a single `transition()` function that: validates the
transition is legal, updates the database, updates the in-memory object, and records an
audit event. Impossible to skip a state or make an invalid transition.

### Circuit Breaker
The bank client is wrapped in a circuit breaker. After N consecutive failures, the
circuit opens and subsequent calls fail immediately instead of waiting for timeout.
After a cooldown period, a test request is allowed through — if it succeeds, the circuit
closes.

## Production Considerations

Things this project demonstrates vs. what a production system would add:

| This Project | Production |
|-------------|------------|
| Simulated bank client by default | Real bank partner API integration |
| In-memory rate limiter | Redis-backed global rate limiter |
| slog JSON logging | OpenTelemetry + Datadog/CloudWatch |
| Single PostgreSQL | Primary + read replicas + PgBouncer |
| Basic API key auth | OAuth2 + mTLS + PCI DSS compliance |
| Manual SQL migrations executed at startup | golang-migrate or Atlas |
| Basic K8s manifests included in repo | Fully hardened deployment specs and autoscaling |

## License

MIT

## API Examples

POST `/payments`

Request:

```json
{
  "merchant_id": "m_123",
  "amount": 100.50,
  "currency": "USD"
}
```

Response:

```json
{
  "success": true,
  "data": {
    "transaction_id": "tx_456",
    "status": "approved"
  }
}
```

Error example:

```json
{
  "success": false,
  "error": {
    "code": "AMOUNT_EXCEEDS_LIMIT",
    "message": "amount exceeds limit"
  }
}
```

## Environment Variables

The repository includes a root-level `.env.example` file with safe placeholder values for local development. It documents the expected environment variables used by the application so you can create a private `.env` file without guessing the required keys.

The example file includes:

- `DB_HOST`
- `DB_PORT`
- `DB_USER`
- `DB_PASSWORD`
- `DB_NAME`
- `PORT`

## Metrics

The API exposes a `GET /metrics` endpoint for Prometheus scraping. It uses the default Prometheus collector set and does not require custom application metrics to be enabled.

This endpoint is intended for operational visibility during local development and production-style deployments.

## Readiness

The API exposes a `GET /ready` endpoint that returns:

```json
{
  "status": "ready"
}
```

It responds with HTTP `200` and is intended for readiness checks.

## API Contract

The HTTP contract is documented in [`/api/openapi.yaml`](/Users/aszender/VSCProyects/Paramount-GO/api/openapi.yaml).

Use that file as the source of truth for endpoint paths, request bodies, and response shapes.

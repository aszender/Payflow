package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/aszender/payflow/internal/config"
	"github.com/aszender/payflow/internal/handler"
	"github.com/aszender/payflow/internal/metrics"
	"github.com/aszender/payflow/internal/middleware"
	"github.com/aszender/payflow/internal/repository/postgres"
	"github.com/aszender/payflow/internal/service"
	"github.com/aszender/payflow/internal/telemetry"
)

const version = "1.0.0"

func main() {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	}))
	slog.SetDefault(logger)

	if cfg.IsProduction() {
		logger.Info("starting in production mode")
	}

	tracingShutdown, err := telemetry.SetupTracing(ctx, telemetry.TraceConfig{
		Enabled:      cfg.TracingEnabled,
		ServiceName:  cfg.TracingServiceName,
		OTLPEndpoint: cfg.TracingOTLPEndpoint,
		OTLPInsecure: cfg.TracingOTLPInsecure,
	})
	if err != nil {
		logger.Error("failed to initialize tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown failed", "error", err)
		}
	}()

	// --- Database ---
	db, err := postgres.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBConnMaxLife)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	logger.Info("database connected")

	// --- Migrations ---
	if err := postgres.RunMigrations(ctx, db.DB, "migrations"); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}

	// --- Repositories ---
	merchantRepo := postgres.NewMerchantRepo(db.DB)
	txRepo := postgres.NewTransactionRepo(db.DB)
	eventRepo := postgres.NewEventRepo(db.DB)
	outboxRepo := postgres.NewOutboxRepo(db.DB)

	var redisClient *redis.Client
	if cfg.RedisAddr != "" {
		redisClient = redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		})
		defer redisClient.Close()

		if err := redisClient.Ping(ctx).Err(); err != nil {
			logger.Warn("redis ping failed; middleware will degrade gracefully", "error", err)
		} else {
			logger.Info("redis connected", "addr", cfg.RedisAddr)
		}
	}

	// --- Metrics ---
	appMetrics := metrics.New()

	// --- Service ---
	bankClient := service.BankClient(&service.SimulatedBankClient{Latency: 150 * time.Millisecond})
	if cfg.BankAPIURL != "" {
		bankClient = service.NewHTTPBankClient(cfg.BankAPIURL, cfg.BankAPITimeout)
	}

	paymentSvc := service.NewPaymentService(service.PaymentServiceConfig{
		DB:             db.DB,
		Merchants:      merchantRepo,
		Transactions:   txRepo,
		Events:         eventRepo,
		Outbox:         outboxRepo,
		Logger:         logger,
		BankClient:     bankClient,
		CircuitBreaker: service.NewCircuitBreaker(cfg.BankCBThreshold, cfg.BankCBReset),
		RetryConfig: service.RetryConfig{
			MaxAttempts: cfg.BankMaxRetries,
			BaseDelay:   cfg.BankRetryBase,
			MaxDelay:    cfg.BankRetryMax,
			Jitter:      cfg.BankRetryJitter,
		},
		BankTimeout:         cfg.BankAPITimeout,
		MaxTransactionCents: cfg.MaxTransactionCents,
	})

	// --- Outbox Worker (background goroutine) ---
	outboxWorker := service.NewOutboxWorker(service.OutboxWorkerConfig{
		Outbox:    outboxRepo,
		Logger:    logger.With("component", "outbox_worker"),
		Interval:  2 * time.Second,
		BatchSize: 100,
	})
	if cfg.KafkaEnabled {
		outboxWorker = service.NewOutboxWorker(service.OutboxWorkerConfig{
			Outbox:    outboxRepo,
			Brokers:   cfg.KafkaBrokers,
			Topic:     cfg.KafkaTopic,
			Logger:    logger.With("component", "outbox_worker"),
			Interval:  1 * time.Second,
			BatchSize: 100,
		})
	}

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	go outboxWorker.Run(workerCtx)

	// --- Handlers ---
	txHandler := handler.NewTransactionHandler(paymentSvc)
	merchantHandler := handler.NewMerchantHandler(paymentSvc)
	healthHandler := handler.NewHealthHandler(db, version)

	// --- Rate Limiter ---
	var limiter middleware.RequestLimiter = middleware.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	if redisClient != nil {
		limiter = middleware.NewRedisRateLimiter(redisClient, float64(cfg.RateLimitRPS), float64(cfg.RateLimitBurst))
	}
	idempotencyStore := middleware.NewIdempotencyStore(redisClient)

	// --- Router ---
	r := chi.NewRouter()

	// Global middleware (applied to ALL routes)
	r.Use(middleware.Recovery(logger))
	r.Use(middleware.RequestID)
	r.Use(middleware.Tracing(cfg.TracingServiceName))
	r.Use(middleware.Logging(logger))
	r.Use(middleware.MetricsMiddleware(appMetrics))
	r.Use(middleware.CORS)
	r.Use(middleware.RateLimit(limiter))

	// Public routes (no auth)
	r.Get("/health", healthHandler.Check)
	r.Get("/ready", healthHandler.Ready)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	// Authenticated API routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.APIKeyAuth(merchantRepo, logger))

		r.Route("/transactions", func(r chi.Router) {
			r.With(middleware.Idempotency(idempotencyStore)).Post("/", txHandler.Create)
			r.Get("/{id}", txHandler.GetByID)
			r.Post("/{id}/refund", txHandler.Refund)
			r.Get("/{id}/events", txHandler.GetEvents)
		})

		r.Route("/merchants", func(r chi.Router) {
			r.Get("/{id}/balance", merchantHandler.GetBalance)
			r.Get("/{id}/transactions", txHandler.ListByMerchant)
		})
	})

	// --- Server ---
	server := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting",
			"addr", cfg.Addr(),
			"version", version,
			"environment", cfg.Environment,
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	var exitCode int

	select {
	case sig := <-quit:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-serverErr:
		logger.Error("server failed", "error", err)
		exitCode = 1
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	workerCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", "error", err)
	}
	logger.Info("server stopped gracefully")

	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func parseLogLevel(raw string) slog.Level {
	switch raw {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

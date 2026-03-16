package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Server
	Port        int
	Environment string
	LogLevel    string

	// Database
	DatabaseURL    string
	DBMaxOpenConns int
	DBMaxIdleConns int
	DBConnMaxLife  time.Duration

	// Kafka
	KafkaBrokers []string
	KafkaTopic   string
	KafkaEnabled bool

	// Rate Limiting
	RateLimitRPS   int
	RateLimitBurst int

	// Payment Processing
	BankAPITimeout    time.Duration
	BankAPIURL        string
	BankMaxRetries    int
	BankRetryBase     time.Duration
	BankRetryMax      time.Duration
	BankRetryJitter   time.Duration
	BankCBThreshold   int
	BankCBReset       time.Duration
	MaxTransactionAmt float64
}

func Load() (*Config, error) {
	port, err := strconv.Atoi(getEnv("PORT", "8080"))
	if err != nil {
		return nil, fmt.Errorf("invalid PORT: %w", err)
	}

	cfg := &Config{
		Port:        port,
		Environment: getEnv("ENVIRONMENT", "development"),
		LogLevel:    getEnv("LOG_LEVEL", "info"),

		DatabaseURL:    getEnv("DATABASE_URL", "postgres://payflow:payflow_secret@localhost:5432/payflow?sslmode=disable"),
		DBMaxOpenConns: getEnvInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns: getEnvInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLife:  time.Duration(getEnvInt("DB_CONN_MAX_LIFE_MIN", 5)) * time.Minute,

		KafkaBrokers: []string{getEnv("KAFKA_BROKERS", "localhost:9092")},
		KafkaTopic:   getEnv("KAFKA_TOPIC", "payment-events"),
		KafkaEnabled: getEnv("KAFKA_ENABLED", "false") == "true",

		RateLimitRPS:   getEnvInt("RATE_LIMIT_RPS", 10),
		RateLimitBurst: getEnvInt("RATE_LIMIT_BURST", 20),

		BankAPITimeout:    time.Duration(getEnvInt("BANK_API_TIMEOUT_SEC", 5)) * time.Second,
		BankAPIURL:        getEnv("BANK_API_URL", ""),
		BankMaxRetries:    getEnvInt("BANK_MAX_RETRIES", 3),
		BankRetryBase:     time.Duration(getEnvInt("BANK_RETRY_BASE_MS", 100)) * time.Millisecond,
		BankRetryMax:      time.Duration(getEnvInt("BANK_RETRY_MAX_MS", 1000)) * time.Millisecond,
		BankRetryJitter:   time.Duration(getEnvInt("BANK_RETRY_JITTER_MS", 50)) * time.Millisecond,
		BankCBThreshold:   getEnvInt("BANK_CB_THRESHOLD", 3),
		BankCBReset:       time.Duration(getEnvInt("BANK_CB_RESET_SEC", 5)) * time.Second,
		MaxTransactionAmt: getEnvFloat("MAX_TRANSACTION_AMOUNT", 25000.00),
	}

	return cfg, nil
}

func (c *Config) Addr() string {
	return fmt.Sprintf(":%d", c.Port)
}

func (c *Config) IsProduction() bool {
	return c.Environment == "production"
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	val := getEnv(key, "")
	if val == "" {
		return fallback
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvFloat(key string, fallback float64) float64 {
	val := getEnv(key, "")
	if val == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return fallback
	}
	return f
}

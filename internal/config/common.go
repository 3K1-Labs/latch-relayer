package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/stellar/go-stellar-sdk/network"
)

// Common is the configuration every relayer binary shares: network, database,
// HTTP server, auth and capacity limits. Each service embeds it in its own
// config type and adds only the keys and settings that service needs.
type Common struct {
	RPCURL            string
	NetworkPassphrase string

	DatabaseURL string
	Port        string

	// Shared secret every caller but /health must present as a bearer token.
	// latch-api is the only intended caller — this is service-to-service, not a
	// user credential. Each service has its own.
	APIKey string

	// Capacity limits
	DBMaxConns       int32
	DBMinConns       int32
	MaxInflight      int     // concurrent requests before new ones get 503
	RateLimitRPS     float64 // per-caller sustained requests/second
	RateLimitBurst   int
	RPCTimeout       time.Duration
	ShutdownDrain    time.Duration // how long in-flight work gets to finish on SIGTERM
	MetricsNamespace string
}

// loadCommon reads and validates the shared settings from the environment.
func loadCommon(defaultPort, metricsNamespace string) (Common, error) {
	c := Common{
		RPCURL:           getEnv("RPC_URL", "https://soroban-testnet.stellar.org"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		Port:             getEnv("PORT", defaultPort),
		APIKey:           os.Getenv("RELAYER_API_KEY"),
		MetricsNamespace: metricsNamespace,
	}

	switch getEnv("NETWORK", "testnet") {
	case "mainnet", "pubnet":
		c.NetworkPassphrase = network.PublicNetworkPassphrase
	default:
		c.NetworkPassphrase = network.TestNetworkPassphrase
	}

	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	// Fail closed. Defaulting to "no auth" when the var is unset is how an
	// internal service ends up publicly mintable after one bad deploy.
	if len(c.APIKey) < 32 {
		return c, errors.New("RELAYER_API_KEY is required and must be at least 32 characters")
	}

	var err error
	if c.DBMaxConns, err = envInt32("DB_MAX_CONNS", 20); err != nil {
		return c, err
	}
	if c.DBMinConns, err = envInt32("DB_MIN_CONNS", 2); err != nil {
		return c, err
	}
	if c.DBMinConns > c.DBMaxConns {
		return c, errors.New("DB_MIN_CONNS must not exceed DB_MAX_CONNS")
	}
	if c.MaxInflight, err = envInt("MAX_INFLIGHT_REQUESTS", 500); err != nil {
		return c, err
	}
	if c.RateLimitRPS, err = envFloat("RATE_LIMIT_RPS", 100); err != nil {
		return c, err
	}
	if c.RateLimitBurst, err = envInt("RATE_LIMIT_BURST", 300); err != nil {
		return c, err
	}
	if c.RPCTimeout, err = envMillis("RPC_TIMEOUT_MS", 15_000); err != nil {
		return c, err
	}
	if c.ShutdownDrain, err = envSeconds("SHUTDOWN_DRAIN_SECONDS", 30); err != nil {
		return c, err
	}
	return c, nil
}

// getEnv returns the env var value or a fallback default.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt parses a positive integer env var, returning fallback when unset.
func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

func envInt32(key string, fallback int32) (int32, error) {
	n, err := envInt(key, int(fallback))
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt32 {
		return 0, fmt.Errorf("%s is too large", key)
	}
	return int32(n), nil
}

func envFloat(key string, fallback float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("%s must be a positive number, got %q", key, v)
	}
	return f, nil
}

func envMillis(key string, fallback int) (time.Duration, error) {
	n, err := envInt(key, fallback)
	return time.Duration(n) * time.Millisecond, err
}

func envSeconds(key string, fallback int) (time.Duration, error) {
	n, err := envInt(key, fallback)
	return time.Duration(n) * time.Second, err
}

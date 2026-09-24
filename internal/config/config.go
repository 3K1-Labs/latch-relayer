package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
)

// PoolAccount holds a single pooled G-address and its loaded signing keypair.
// The relayer monitors each pool address on its own Horizon SSE stream.
type PoolAccount struct {
	Address string
	Keypair *keypair.Full
}

// Config holds all runtime configuration for the relayer.
// It is loaded once at startup via Load() and passed as a dependency.
type Config struct {
	// Stellar network
	HorizonURL        string
	RPCURL            string
	NetworkPassphrase string

	// Pool accounts — one SSE watcher goroutine per account
	PoolAccounts    []PoolAccount
	RecoveryAddress string

	// PostgreSQL
	DatabaseURL string

	// HTTP server
	Port string

	// RetryInterval is how often the background worker sweeps forwards that are
	// waiting for another attempt. It bounds how long a deposit that lost the
	// race for its pool account's ledger slot waits before trying again, so the
	// old 30s default meant a straggler could sit half a minute waiting for a
	// slot that was already free.
	RetryInterval time.Duration

	// Shared secret every caller but /health must present as a bearer token.
	// latch-api is the only intended caller — this is service-to-service, not a
	// user credential.
	APIKey string
}

// Load reads environment variables (and an optional .env file), validates all
// required values, and returns a populated Config. Fails fast on missing fields.
func Load() (*Config, error) {
	// .env is optional — present in local dev, absent in production containers
	godotenv.Load()

	cfg := &Config{
		HorizonURL:      getEnv("HORIZON_URL", "https://horizon-testnet.stellar.org"),
		RPCURL:          getEnv("RPC_URL", "https://soroban-testnet.stellar.org"),
		RecoveryAddress: os.Getenv("RECOVERY_ADDRESS"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Port:            getEnv("PORT", "4000"),
		RetryInterval:   time.Duration(getEnvInt("RETRY_INTERVAL_SEC", 10)) * time.Second,
		APIKey:          os.Getenv("RELAYER_API_KEY"),
	}

	// Set network passphrase from NETWORK env var
	switch getEnv("NETWORK", "testnet") {
	case "mainnet", "pubnet":
		cfg.NetworkPassphrase = network.PublicNetworkPassphrase
	default:
		cfg.NetworkPassphrase = network.TestNetworkPassphrase
	}

	// Validate required fields
	if cfg.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if cfg.RecoveryAddress == "" {
		return nil, errors.New("RECOVERY_ADDRESS is required")
	}
	// Fail closed. Defaulting to "no auth" when the var is unset is how an
	// internal service ends up publicly mintable after one bad deploy.
	if len(cfg.APIKey) < 32 {
		return nil, errors.New("RELAYER_API_KEY is required and must be at least 32 characters")
	}

	// Load pool accounts — indexed as POOL_ADDRESS_1 / POOL_PRIVATE_KEY_1, etc.
	// Stops at the first missing pair.
	for i := 1; ; i++ {
		addr := os.Getenv(fmt.Sprintf("POOL_ADDRESS_%d", i))
		seed := os.Getenv(fmt.Sprintf("POOL_PRIVATE_KEY_%d", i))

		if addr == "" || seed == "" {
			break
		}

		kp, err := keypair.ParseFull(seed)
		if err != nil {
			return nil, fmt.Errorf("POOL_PRIVATE_KEY_%d is invalid: %w", i, err)
		}
		if kp.Address() != addr {
			return nil, fmt.Errorf("POOL_ADDRESS_%d does not match POOL_PRIVATE_KEY_%d", i, i)
		}

		cfg.PoolAccounts = append(cfg.PoolAccounts, PoolAccount{
			Address: addr,
			Keypair: kp,
		})
	}

	if len(cfg.PoolAccounts) == 0 {
		return nil, errors.New("at least one pool account is required (POOL_ADDRESS_1 + POOL_PRIVATE_KEY_1)")
	}

	return cfg, nil
}

// getEnv returns the env var value or a fallback default.
// getEnvInt parses an integer env var, falling back on unset or malformed
// input rather than failing startup over a tuning value.
func getEnvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/keypair"
)

// PoolAccount holds a single pooled G-address and its loaded signing keypair.
// The relayer monitors each pool address on its own Horizon SSE stream.
type PoolAccount struct {
	Address string
	Keypair *keypair.Full
}

// Config is the deposit bridge's configuration (cmd/serve). It embeds Common,
// so shared fields are reached directly: cfg.APIKey, cfg.RPCURL, ...
type Config struct {
	Common

	HorizonURL string

	// Pool accounts — one SSE watcher goroutine per account
	PoolAccounts    []PoolAccount
	RecoveryAddress string

	// RetryInterval is how often the background worker sweeps forwards that are
	// waiting for another attempt. It bounds how long a deposit that lost the
	// race for its pool account's ledger slot waits before trying again, so the
	// old 30s default meant a straggler could sit half a minute waiting for a
	// slot that was already free.
	RetryInterval time.Duration
}

// Load reads environment variables (and an optional .env file), validates all
// required values, and returns the deposit bridge's Config. Fails fast on
// missing fields.
func Load() (*Config, error) {
	// .env is optional — present in local dev, absent in production containers
	godotenv.Load()

	common, err := loadCommon("4000", "latch_relayer")
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Common:          common,
		HorizonURL:      getEnv("HORIZON_URL", "https://horizon-testnet.stellar.org"),
		RecoveryAddress: os.Getenv("RECOVERY_ADDRESS"),
	}
	if cfg.RecoveryAddress == "" {
		return nil, errors.New("RECOVERY_ADDRESS is required")
	}
	if cfg.RetryInterval, err = envSeconds("RETRY_INTERVAL_SEC", 10); err != nil {
		return nil, err
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

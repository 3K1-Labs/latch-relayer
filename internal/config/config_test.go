package config

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/network"
)

// validEnv sets the minimum environment required for Load() to succeed.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/latch_relayer")
	t.Setenv("RECOVERY_ADDRESS", "GBXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")
	t.Setenv("POOL_ADDRESS_1", "GB3AETG6Q5SYNM36TPHULYXPP364EC77YJJBDMNKDA5CF4QYP3XHJ6I5")
	t.Setenv("POOL_PRIVATE_KEY_1", "SDUCBCFL3QJXQM4EGZWW7UUJYEDLXD3OYXJ3JAA3VPNNOINFLPT6R7G6")
	t.Setenv("RELAYER_API_KEY", "0123456789abcdef0123456789abcdef")
}

func TestLoad_Success(t *testing.T) {
	validEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.PoolAccounts) != 1 {
		t.Fatalf("expected 1 pool account, got %d", len(cfg.PoolAccounts))
	}
	if cfg.PoolAccounts[0].Address != "GB3AETG6Q5SYNM36TPHULYXPP364EC77YJJBDMNKDA5CF4QYP3XHJ6I5" {
		t.Fatalf("unexpected pool address: %s", cfg.PoolAccounts[0].Address)
	}
	if cfg.NetworkPassphrase != network.TestNetworkPassphrase {
		t.Fatalf("expected testnet passphrase, got: %s", cfg.NetworkPassphrase)
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	validEnv(t)
	t.Setenv("DATABASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DATABASE_URL")
	}
}

func TestLoad_MissingRecoveryAddress(t *testing.T) {
	validEnv(t)
	t.Setenv("RECOVERY_ADDRESS", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing RECOVERY_ADDRESS")
	}
}

// The relayer must refuse to start without a key rather than fall back to
// serving unauthenticated — that fallback is how it ends up publicly mintable.
func TestLoad_MissingAPIKey(t *testing.T) {
	validEnv(t)
	t.Setenv("RELAYER_API_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing RELAYER_API_KEY")
	}
}

func TestLoad_ShortAPIKey(t *testing.T) {
	validEnv(t)
	t.Setenv("RELAYER_API_KEY", "too-short")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for a RELAYER_API_KEY under 32 characters")
	}
}

func TestLoad_NoPoolAccounts(t *testing.T) {
	validEnv(t)
	t.Setenv("POOL_ADDRESS_1", "")
	t.Setenv("POOL_PRIVATE_KEY_1", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when no pool accounts configured")
	}
}

func TestLoad_KeyAddressMismatch(t *testing.T) {
	validEnv(t)
	// wrong address for the key
	t.Setenv("POOL_ADDRESS_1", "GBADADDRESSXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when address does not match private key")
	}
}

func TestLoad_MainnetPassphrase(t *testing.T) {
	validEnv(t)
	t.Setenv("NETWORK", "mainnet")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.NetworkPassphrase != network.PublicNetworkPassphrase {
		t.Fatalf("expected mainnet passphrase, got: %s", cfg.NetworkPassphrase)
	}
}

func TestLoad_CapacityDefaults(t *testing.T) {
	validEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.DBMaxConns != 20 || cfg.DBMinConns != 2 || cfg.MaxInflight != 500 ||
		cfg.RateLimitRPS != 100 || cfg.RateLimitBurst != 300 ||
		cfg.RPCTimeout != 15*time.Second || cfg.ShutdownDrain != 30*time.Second {
		t.Fatalf("unexpected capacity defaults: %+v", cfg)
	}
}

func TestLoad_CapacityOverrides(t *testing.T) {
	validEnv(t)
	t.Setenv("DB_MAX_CONNS", "40")
	t.Setenv("MAX_INFLIGHT_REQUESTS", "1000")
	t.Setenv("RATE_LIMIT_RPS", "12.5")
	t.Setenv("RPC_TIMEOUT_MS", "2500")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.DBMaxConns != 40 || cfg.MaxInflight != 1000 || cfg.RateLimitRPS != 12.5 || cfg.RPCTimeout != 2500*time.Millisecond {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoad_InvalidCapacityValues(t *testing.T) {
	cases := map[string]string{
		"DB_MAX_CONNS":           "zero",
		"MAX_INFLIGHT_REQUESTS":  "-5",
		"RATE_LIMIT_RPS":         "0",
		"SHUTDOWN_DRAIN_SECONDS": "1.5",
	}
	for key, val := range cases {
		t.Run(key, func(t *testing.T) {
			validEnv(t)
			t.Setenv(key, val)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q", key, val)
			}
		})
	}
}

func TestLoad_MinConnsAboveMax(t *testing.T) {
	validEnv(t)
	t.Setenv("DB_MAX_CONNS", "5")
	t.Setenv("DB_MIN_CONNS", "6")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when DB_MIN_CONNS > DB_MAX_CONNS")
	}
}

func TestLoad_DefaultPort(t *testing.T) {
	validEnv(t)
	t.Setenv("PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != "4000" {
		t.Fatalf("expected default port 4000, got %s", cfg.Port)
	}
}

func TestLoad_RetryInterval(t *testing.T) {
	validEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.RetryInterval != 10*time.Second {
		t.Fatalf("default RetryInterval = %v, want 10s", cfg.RetryInterval)
	}

	// A typo fails the boot like every other setting, rather than silently
	// running with the default.
	t.Setenv("RETRY_INTERVAL_SEC", "ten")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for a non-numeric RETRY_INTERVAL_SEC")
	}
}

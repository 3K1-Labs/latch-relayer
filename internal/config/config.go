package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/latch/relayer/internal/gasless/keys"
)

// Circle's USDC issuers. Accepted by default on the matching network.
const (
	USDCIssuerMainnet = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	USDCIssuerTestnet = "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
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

	// AcceptedAssets are the assets deposits may be credited in, as the
	// watcher writes them: "native" or "CODE:ISSUER". A deposit in anything
	// else is swept to recovery instead of forwarded. Defaults to XLM and
	// Circle's USDC for the configured network (ACCEPTED_ASSETS overrides).
	AcceptedAssets []string

	// Channels are the deposit bridge's channel accounts (#48), derived from
	// DEPOSIT_CHANNEL_SEED. Empty (DEPOSIT_CHANNEL_COUNT unset or 0) keeps the
	// pool as every transaction's source: one transaction per pool per ledger.
	Channels []keys.Channel
	// InstanceID identifies this process in channel leases.
	InstanceID string

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
	if cfg.AcceptedAssets, err = acceptedAssets(os.Getenv("ACCEPTED_ASSETS"), cfg.NetworkPassphrase); err != nil {
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

	if cfg.Channels, err = depositChannels(cfg); err != nil {
		return nil, err
	}
	cfg.InstanceID = os.Getenv("INSTANCE_ID")
	if cfg.InstanceID == "" {
		host, _ := os.Hostname()
		cfg.InstanceID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}

	return cfg, nil
}

// acceptedAssets parses ACCEPTED_ASSETS, a comma-separated list of "native" and
// "CODE:ISSUER" entries. Empty means XLM plus Circle's USDC on the configured
// network.
func acceptedAssets(raw, passphrase string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		issuer := USDCIssuerTestnet
		if passphrase == network.PublicNetworkPassphrase {
			issuer = USDCIssuerMainnet
		}
		return []string{"native", "USDC:" + issuer}, nil
	}
	var out []string
	for _, a := range strings.Split(raw, ",") {
		a = strings.TrimSpace(a)
		if a == "native" {
			out = append(out, a)
			continue
		}
		code, issuer, ok := strings.Cut(a, ":")
		if !ok || code == "" || len(code) > 12 || !strkey.IsValidEd25519PublicKey(issuer) {
			return nil, fmt.Errorf("ACCEPTED_ASSETS: %q is not \"native\" or CODE:ISSUER", a)
		}
		out = append(out, a)
	}
	return out, nil
}

// depositChannels derives the deposit bridge's channel accounts from
// DEPOSIT_CHANNEL_SEED when DEPOSIT_CHANNEL_COUNT is positive. The seed must
// differ from the gasless service's CHANNEL_SEED: the two services lease from
// separate tables, and sharing a seed would have both send from the same
// accounts and collide on sequence numbers.
func depositChannels(cfg *Config) ([]keys.Channel, error) {
	if v := strings.TrimSpace(os.Getenv("DEPOSIT_CHANNEL_COUNT")); v == "" || v == "0" {
		return nil, nil
	}
	count, err := envInt("DEPOSIT_CHANNEL_COUNT", 0)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(os.Getenv("DEPOSIT_CHANNEL_SEED"))
	if err != nil || len(seed) < keys.MinSeedBytes {
		return nil, fmt.Errorf("DEPOSIT_CHANNEL_SEED is required when DEPOSIT_CHANNEL_COUNT > 0: hex, at least %d bytes (generate with: openssl rand -hex 32)", keys.MinSeedBytes)
	}
	if gasless := os.Getenv("CHANNEL_SEED"); gasless != "" && strings.EqualFold(gasless, os.Getenv("DEPOSIT_CHANNEL_SEED")) {
		return nil, errors.New("DEPOSIT_CHANNEL_SEED must differ from the gasless service's CHANNEL_SEED")
	}
	chans, err := keys.DeriveChannels(seed, count)
	if err != nil {
		return nil, err
	}
	for _, ch := range chans {
		if ch.Address() == cfg.RecoveryAddress {
			return nil, fmt.Errorf("deposit channel %d derives to RECOVERY_ADDRESS; use a different DEPOSIT_CHANNEL_SEED", ch.Index)
		}
		for _, p := range cfg.PoolAccounts {
			if ch.Address() == p.Address {
				return nil, fmt.Errorf("deposit channel %d derives to pool %s; use a different DEPOSIT_CHANNEL_SEED", ch.Index, p.Address)
			}
		}
	}
	return chans, nil
}

package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/amount"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/latch/relayer/internal/gasless/keys"
)

// Gasless is the gasless sponsor's configuration (cmd/gasless).
//
// Key model: one executor (holds the FeeForwarder `executor` role and signs
// the relayer's authorization entry), one funder (pays every fee-bump; holds
// the XLM float), and N channel accounts derived from CHANNEL_SEED (powerless
// transaction sources, one in-flight transaction each).
type Gasless struct {
	Common

	FeeForwarderID string // C-address of the FeeForwarder contract

	Executor *keypair.Full
	Funder   *keypair.Full
	Channels []keys.Channel

	FunderMinStroops     int64 // below this, sponsorship is refused
	ChannelMinStroops    int64 // below this, a channel is taken out of rotation
	BalanceCheckInterval time.Duration

	InstanceID string // identifies this process in channel leases
}

// forbiddenInGasless are deposit-bridge settings. The gasless process must
// never hold a pool key: a leaked pool key drains user deposits, so it stays
// in the deposit process only.
var forbiddenInGasless = []string{"POOL_ADDRESS_", "POOL_PRIVATE_KEY_", "RECOVERY_ADDRESS"}

// GaslessEnvFile is the gasless service's local env file — separate from the
// deposit bridge's .env so the two never share keys by accident. The name
// ends in ".env" so the existing *.env gitignore rule covers it.
const GaslessEnvFile = "gasless.env"

// LoadGasless reads the gasless service's settings from the environment (and
// an optional gasless.env file).
func LoadGasless() (*Gasless, error) {
	godotenv.Load(GaslessEnvFile)

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		for _, prefix := range forbiddenInGasless {
			if strings.HasPrefix(name, prefix) {
				return nil, fmt.Errorf("%s is set: pool keys and deposit settings must not be present in the gasless service", name)
			}
		}
	}

	common, err := loadCommon("4001", "latch_gasless")
	if err != nil {
		return nil, err
	}
	cfg := &Gasless{Common: common, FeeForwarderID: os.Getenv("FEE_FORWARDER_CONTRACT_ID")}

	if !strkey.IsValidContractAddress(cfg.FeeForwarderID) {
		return nil, errors.New("FEE_FORWARDER_CONTRACT_ID is required and must be a C-address")
	}
	if cfg.Executor, err = loadKeypair("EXECUTOR"); err != nil {
		return nil, err
	}
	if cfg.Funder, err = loadKeypair("FUNDER"); err != nil {
		return nil, err
	}
	if cfg.Executor.Address() == cfg.Funder.Address() {
		return nil, errors.New("EXECUTOR and FUNDER must be different accounts")
	}

	seed, err := hex.DecodeString(os.Getenv("CHANNEL_SEED"))
	if err != nil || len(seed) < keys.MinSeedBytes {
		return nil, fmt.Errorf("CHANNEL_SEED is required: hex, at least %d bytes (generate with: openssl rand -hex 32)", keys.MinSeedBytes)
	}
	count, err := envInt("CHANNEL_COUNT", 0)
	if err != nil || count == 0 {
		return nil, errors.New("CHANNEL_COUNT is required and must be a positive integer")
	}
	if cfg.Channels, err = keys.DeriveChannels(seed, count); err != nil {
		return nil, err
	}
	for _, ch := range cfg.Channels {
		if a := ch.Address(); a == cfg.Executor.Address() || a == cfg.Funder.Address() {
			return nil, fmt.Errorf("channel %d derives to the executor or funder address; use a different CHANNEL_SEED", ch.Index)
		}
	}

	if cfg.FunderMinStroops, err = envXLM("FUNDER_MIN_XLM", "50"); err != nil {
		return nil, err
	}
	// Channels never pay fees (the funder does, via fee-bump); they only need
	// to exist, which takes the 2 × 0.5 XLM base reserve.
	if cfg.ChannelMinStroops, err = envXLM("CHANNEL_MIN_XLM", "1"); err != nil {
		return nil, err
	}
	if cfg.BalanceCheckInterval, err = envSeconds("BALANCE_CHECK_SECONDS", 60); err != nil {
		return nil, err
	}

	cfg.InstanceID = os.Getenv("INSTANCE_ID")
	if cfg.InstanceID == "" {
		host, _ := os.Hostname()
		cfg.InstanceID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return cfg, nil
}

// loadKeypair reads <PREFIX>_ADDRESS and <PREFIX>_PRIVATE_KEY and checks they match.
func loadKeypair(prefix string) (*keypair.Full, error) {
	addr := os.Getenv(prefix + "_ADDRESS")
	seed := os.Getenv(prefix + "_PRIVATE_KEY")
	if addr == "" || seed == "" {
		return nil, fmt.Errorf("%s_ADDRESS and %s_PRIVATE_KEY are required", prefix, prefix)
	}
	kp, err := keypair.ParseFull(seed)
	if err != nil {
		return nil, fmt.Errorf("%s_PRIVATE_KEY is invalid: %w", prefix, err)
	}
	if kp.Address() != addr {
		return nil, fmt.Errorf("%s_ADDRESS does not match %s_PRIVATE_KEY", prefix, prefix)
	}
	return kp, nil
}

// envXLM parses a decimal XLM amount ("50", "1.5") into stroops.
func envXLM(key, fallback string) (int64, error) {
	v := getEnv(key, fallback)
	stroops, err := amount.ParseInt64(v)
	if err != nil || stroops <= 0 {
		return 0, fmt.Errorf("%s must be a positive XLM amount, got %q", key, v)
	}
	return stroops, nil
}

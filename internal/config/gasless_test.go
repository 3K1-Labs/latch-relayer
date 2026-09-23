package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
)

// validGaslessEnv sets the minimum environment LoadGasless needs.
func validGaslessEnv(t *testing.T) (executor, funder *keypair.Full) {
	t.Helper()
	executor, funder = keypair.MustRandom(), keypair.MustRandom()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/latch_gasless")
	t.Setenv("RELAYER_API_KEY", "fedcba9876543210fedcba9876543210")
	t.Setenv("FEE_FORWARDER_CONTRACT_ID", "CB6KFDFN7CXIOSBOEXABOX6KPRK4X6VMPSDWCGYP564JXGN2LIH2QPWI")
	t.Setenv("EXECUTOR_ADDRESS", executor.Address())
	t.Setenv("EXECUTOR_PRIVATE_KEY", executor.Seed())
	t.Setenv("FUNDER_ADDRESS", funder.Address())
	t.Setenv("FUNDER_PRIVATE_KEY", funder.Seed())
	t.Setenv("CHANNEL_SEED", "000102030405060708090a0b0c0d0e0f")
	t.Setenv("CHANNEL_COUNT", "3")
	return executor, funder
}

func TestLoadGasless_Success(t *testing.T) {
	executor, funder := validGaslessEnv(t)

	cfg, err := LoadGasless()
	if err != nil {
		t.Fatalf("LoadGasless() error: %v", err)
	}
	if cfg.Executor.Address() != executor.Address() || cfg.Funder.Address() != funder.Address() {
		t.Fatal("executor/funder not loaded")
	}
	if len(cfg.Channels) != 3 || cfg.Channels[0].Address() != "GCWSJRG6YZSA374IY7LF53PIGTO6JD6BP5CNMUAVNWL3YYE636F3APML" {
		t.Fatalf("channels not derived from seed: %+v", cfg.Channels)
	}
	if cfg.Port != "4001" || cfg.MetricsNamespace != "latch_gasless" {
		t.Fatalf("gasless defaults not applied: port=%s ns=%s", cfg.Port, cfg.MetricsNamespace)
	}
	if cfg.FunderMinStroops != 50*10_000_000 || cfg.ChannelMinStroops != 10_000_000 || cfg.BalanceCheckInterval != time.Minute {
		t.Fatalf("balance defaults wrong: %+v", cfg)
	}
	if cfg.InstanceID == "" {
		t.Fatal("instance id not defaulted")
	}
}

// The whole point of the service split: a pool key must never be loaded into
// the gasless process, even by accident through a shared env file.
func TestLoadGasless_RefusesDepositSettings(t *testing.T) {
	for _, name := range []string{"POOL_ADDRESS_1", "POOL_PRIVATE_KEY_2", "RECOVERY_ADDRESS"} {
		t.Run(name, func(t *testing.T) {
			validGaslessEnv(t)
			t.Setenv(name, "anything")
			_, err := LoadGasless()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("expected refusal naming %s, got %v", name, err)
			}
		})
	}
}

func TestLoadGasless_Validation(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"missing forwarder": func(t *testing.T) { t.Setenv("FEE_FORWARDER_CONTRACT_ID", "") },
		"forwarder not C":   func(t *testing.T) { t.Setenv("FEE_FORWARDER_CONTRACT_ID", keypair.MustRandom().Address()) },
		"executor mismatch": func(t *testing.T) { t.Setenv("EXECUTOR_ADDRESS", keypair.MustRandom().Address()) },
		"missing funder":    func(t *testing.T) { t.Setenv("FUNDER_PRIVATE_KEY", "") },
		"short seed":        func(t *testing.T) { t.Setenv("CHANNEL_SEED", "0001") },
		"seed not hex":      func(t *testing.T) { t.Setenv("CHANNEL_SEED", "not-hex-not-hex-not-hex-not-hex!") },
		"no channel count":  func(t *testing.T) { t.Setenv("CHANNEL_COUNT", "") },
		"bad funder floor":  func(t *testing.T) { t.Setenv("FUNDER_MIN_XLM", "lots") },
		"executor is funder": func(t *testing.T) {
			kp := keypair.MustRandom()
			t.Setenv("EXECUTOR_ADDRESS", kp.Address())
			t.Setenv("EXECUTOR_PRIVATE_KEY", kp.Seed())
			t.Setenv("FUNDER_ADDRESS", kp.Address())
			t.Setenv("FUNDER_PRIVATE_KEY", kp.Seed())
		},
		"channel is executor": func(t *testing.T) {
			// Channel 0 of the test seed, reused as the executor.
			ch0 := "SB6VZS57IY25334Y6F6SPGFUNESWS7D2OSJHKDPIZ354BK3FN5GBTS6V"
			t.Setenv("EXECUTOR_ADDRESS", keypair.MustParseFull(ch0).Address())
			t.Setenv("EXECUTOR_PRIVATE_KEY", ch0)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			validGaslessEnv(t)
			mutate(t)
			if _, err := LoadGasless(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

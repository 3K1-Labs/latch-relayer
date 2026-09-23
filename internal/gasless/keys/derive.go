// Package keys derives the gasless service's channel-account keypairs.
//
// Channel accounts are powerless transaction sources (no contract role, only
// their base reserve), so they're derived from one seed with SEP-0005 paths
// m/44'/148'/i' instead of being configured one by one. Backing up the seed
// backs up every channel; adding capacity is just raising the count.
package keys

import (
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/tools/stellar-hd-wallet/crypto/derivation"
)

// MinSeedBytes is BIP-32's minimum seed length (128 bits).
const MinSeedBytes = 16

// Channel is one derived channel account.
type Channel struct {
	Index   int
	Keypair *keypair.Full
}

func (c Channel) Address() string { return c.Keypair.Address() }

// DeriveChannel derives the keypair at m/44'/148'/index'.
func DeriveChannel(seed []byte, index int) (Channel, error) {
	if len(seed) < MinSeedBytes {
		return Channel{}, fmt.Errorf("channel seed must be at least %d bytes, got %d", MinSeedBytes, len(seed))
	}
	if index < 0 {
		return Channel{}, errors.New("channel index must not be negative")
	}
	key, err := derivation.DeriveForPath(fmt.Sprintf(derivation.StellarAccountPathFormat, index), seed)
	if err != nil {
		return Channel{}, fmt.Errorf("derive channel %d: %w", index, err)
	}
	kp, err := keypair.FromRawSeed(key.RawSeed())
	if err != nil {
		return Channel{}, fmt.Errorf("channel %d keypair: %w", index, err)
	}
	return Channel{Index: index, Keypair: kp}, nil
}

// DeriveChannels derives channels 0..count-1.
func DeriveChannels(seed []byte, count int) ([]Channel, error) {
	out := make([]Channel, 0, count)
	for i := 0; i < count; i++ {
		ch, err := DeriveChannel(seed, i)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

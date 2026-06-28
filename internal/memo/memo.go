package memo

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strconv"

	sdkstrkey "github.com/stellar/go-stellar-sdk/strkey"
)

var (
	ErrNotMemoID      = errors.New("memo type is not MEMO_ID")
	ErrInvalidMemoID  = errors.New("memo value is not a valid uint64")
	ErrInvalidAddress = errors.New("invalid Stellar G-address")
	ErrInvalidCAddress = errors.New("invalid Soroban C-address")
)

// ValidateCAddress returns nil if addr is a valid Soroban contract (C...) address.
func ValidateCAddress(addr string) error {
	if _, err := sdkstrkey.Decode(sdkstrkey.VersionByteContract, addr); err != nil {
		return ErrInvalidCAddress
	}
	return nil
}

// DeriveID deterministically derives a uint64 memo ID from a Soroban C-address.
// It takes the first 8 bytes of the SHA-256 hash of the address string.
// The same C-address always produces the same memo ID — no DB needed to re-derive it.
func DeriveID(cAddress string) uint64 {
	hash := sha256.Sum256([]byte(cAddress))
	return binary.BigEndian.Uint64(hash[:8])
}

// ParseID parses a memo ID from a Horizon payment event.
// Horizon returns memos as (memoType string, memo string).
// We only accept MEMO_ID type ("id") — all others are rejected.
func ParseID(memoType, memo string) (uint64, error) {
	if memoType != "id" {
		return 0, ErrNotMemoID
	}
	id, err := strconv.ParseUint(memo, 10, 64)
	if err != nil {
		return 0, ErrInvalidMemoID
	}
	return id, nil
}

// ToMuxedAddress encodes a pooled G-address + memo ID into an M-address (SEP-23).
// Muxed accounts are not yet universally supported, but memo IDs double as muxed IDs —
// so this gives us a zero-migration upgrade path when support matures.
func ToMuxedAddress(gAddress string, id uint64) (string, error) {
	raw, err := sdkstrkey.Decode(sdkstrkey.VersionByteAccountID, gAddress)
	if err != nil {
		return "", ErrInvalidAddress
	}

	// SEP-23 muxed account payload: 8-byte big-endian uint64 ID + 32-byte public key
	payload := make([]byte, 40)
	binary.BigEndian.PutUint64(payload[:8], id)
	copy(payload[8:], raw)

	return sdkstrkey.Encode(sdkstrkey.VersionByteMuxedAccount, payload)
}

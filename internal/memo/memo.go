package memo

import (
	"errors"
	"strconv"

	sdkstrkey "github.com/stellar/go-stellar-sdk/strkey"
)

var (
	ErrNotMemoID       = errors.New("memo type is not MEMO_ID")
	ErrInvalidMemoID   = errors.New("memo value is not a valid uint64")
	ErrInvalidCAddress = errors.New("invalid Soroban C-address")
)

// ValidateCAddress returns nil if addr is a valid Soroban contract (C...) address.
func ValidateCAddress(addr string) error {
	if _, err := sdkstrkey.Decode(sdkstrkey.VersionByteContract, addr); err != nil {
		return ErrInvalidCAddress
	}
	return nil
}

// ParseID parses a MEMO_ID from a Horizon payment event.
// Returns ErrNotMemoID if the memo type is not "id", ErrInvalidMemoID if the value
// is not a valid uint64.
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

package memo

import (
	"errors"
	"strconv"
	"strings"

	sdkstrkey "github.com/stellar/go-stellar-sdk/strkey"
)

var (
	ErrUnsupportedMemoType = errors.New("memo type is not MEMO_ID or MEMO_TEXT")
	ErrInvalidMemoID       = errors.New("memo value is not a valid uint64")
	ErrInvalidCAddress     = errors.New("invalid Soroban C-address")
)

// ValidateCAddress returns nil if addr is a valid Soroban contract (C...) address.
func ValidateCAddress(addr string) error {
	if _, err := sdkstrkey.Decode(sdkstrkey.VersionByteContract, addr); err != nil {
		return ErrInvalidCAddress
	}
	return nil
}

// ParseID parses an intent memo from a Horizon payment event.
//
// Both MEMO_ID and MEMO_TEXT are accepted. On-ramps and wallet UIs routinely send
// the tag as text even when the digits are a valid uint64 — MoonPay documents the
// XLM tag only as "alpha-numeric" — and a memo refused here is swept to the
// recovery address instead of being credited to the depositor. Memo IDs are random
// uint64s, so a text memo that happens to collide with a live intent is not a
// realistic concern.
//
// The value must still parse as a uint64. Returns ErrUnsupportedMemoType for any
// other memo type (none/hash/return), ErrInvalidMemoID if the value is not a uint64.
func ParseID(memoType, memo string) (uint64, error) {
	if memoType != "id" && memoType != "text" {
		return 0, ErrUnsupportedMemoType
	}
	// A tag copied out of a provider's UI often carries surrounding whitespace.
	id, err := strconv.ParseUint(strings.TrimSpace(memo), 10, 64)
	if err != nil {
		return 0, ErrInvalidMemoID
	}
	return id, nil
}

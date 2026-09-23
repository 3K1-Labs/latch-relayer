package keys

import (
	"encoding/hex"
	"testing"
)

// Vectors from go-stellar-sdk's tools/stellar-hd-wallet/crypto/derivation
// tests (seed 000102…0f), which match SEP-0005.
func TestDeriveChannelsMatchesSEP0005Vectors(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	want := []string{
		"GCWSJRG6YZSA374IY7LF53PIGTO6JD6BP5CNMUAVNWL3YYE636F3APML",
		"GDGYXMH2GBB6E4Z4ZW4APZ7JQTEBNGDAVOWBYEQVSAHA27HXYPHLY5GO",
		"GBUKOZ5272DZQR5CT5H5OCA4FTSRYXO6N56VHLX3BR4QQKIGGMVL6JJV",
	}

	chans, err := DeriveChannels(seed, len(want))
	if err != nil {
		t.Fatal(err)
	}
	for i, ch := range chans {
		if ch.Index != i || ch.Address() != want[i] {
			t.Errorf("channel %d = (%d, %s), want (%d, %s)", i, ch.Index, ch.Address(), i, want[i])
		}
	}
	if chans[0].Keypair.Seed() != "SB6VZS57IY25334Y6F6SPGFUNESWS7D2OSJHKDPIZ354BK3FN5GBTS6V" {
		t.Errorf("channel 0 secret does not match vector")
	}
}

func TestDeriveChannelRejectsShortSeed(t *testing.T) {
	if _, err := DeriveChannel(make([]byte, MinSeedBytes-1), 0); err == nil {
		t.Fatal("expected error for a seed under 16 bytes")
	}
}

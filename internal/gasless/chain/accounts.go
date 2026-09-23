package chain

import (
	"context"
	"fmt"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Stellar RPC's getLedgerEntries accepts at most this many keys per call.
const maxLedgerKeysPerCall = 200

// Account is an account's on-chain state. Exists is false when the account
// has never been created (or was merged away).
type Account struct {
	Exists         bool
	BalanceStroops int64
	Seq            int64
}

// Accounts loads many accounts in batched getLedgerEntries calls — one round
// trip per 200 accounts instead of one per account, which matters once there
// are hundreds of channels.
func Accounts(ctx context.Context, rpc RPC, addresses []string) (map[string]Account, error) {
	out := make(map[string]Account, len(addresses))
	keyToAddr := make(map[string]string, len(addresses))
	keys := make([]string, 0, len(addresses))

	for _, addr := range addresses {
		aid, err := xdr.AddressToAccountId(addr)
		if err != nil {
			return nil, fmt.Errorf("account %q: %w", addr, err)
		}
		lk, err := aid.LedgerKey()
		if err != nil {
			return nil, fmt.Errorf("ledger key for %s: %w", addr, err)
		}
		key, err := xdr.MarshalBase64(lk)
		if err != nil {
			return nil, err
		}
		keyToAddr[key] = addr
		keys = append(keys, key)
		out[addr] = Account{} // absent from the response ⇒ doesn't exist
	}

	for start := 0; start < len(keys); start += maxLedgerKeysPerCall {
		end := min(start+maxLedgerKeysPerCall, len(keys))
		resp, err := rpc.GetLedgerEntries(ctx, protocol.GetLedgerEntriesRequest{Keys: keys[start:end]})
		if err != nil {
			return nil, fmt.Errorf("getLedgerEntries: %w", err)
		}
		for _, e := range resp.Entries {
			addr, ok := keyToAddr[e.KeyXDR]
			if !ok {
				continue
			}
			var data xdr.LedgerEntryData
			if err := xdr.SafeUnmarshalBase64(e.DataXDR, &data); err != nil {
				return nil, fmt.Errorf("decode account %s: %w", addr, err)
			}
			acc, ok := data.GetAccount()
			if !ok {
				return nil, fmt.Errorf("ledger entry for %s is not an account", addr)
			}
			out[addr] = Account{Exists: true, BalanceStroops: int64(acc.Balance), Seq: int64(acc.SeqNum)}
		}
	}
	return out, nil
}

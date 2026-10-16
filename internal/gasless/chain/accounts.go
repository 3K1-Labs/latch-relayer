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

// TokenBalances reads holder's balance in each Stellar Asset Contract in
// tokens with one getLedgerEntries call, keyed by token contract. A SAC
// stores a contract holder's balance as persistent contract data under
// Vec[Symbol("Balance"), holder] with value {amount, authorized, clawback};
// a missing entry is a zero balance.
func TokenBalances(ctx context.Context, rpc RPC, holder string, tokens []string) (map[string]int64, error) {
	holderVal, err := ScAddressVal(holder)
	if err != nil {
		return nil, fmt.Errorf("holder: %w", err)
	}
	vec := xdr.ScVec{ScSymbol("Balance"), holderVal}
	vecPtr := &vec
	balanceKey := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vecPtr}

	out := make(map[string]int64, len(tokens))
	keyToToken := make(map[string]string, len(tokens))
	keys := make([]string, 0, len(tokens))
	for _, token := range tokens {
		tokenAddr, err := ScAddressFromString(token)
		if err != nil {
			return nil, fmt.Errorf("token: %w", err)
		}
		key, err := xdr.MarshalBase64(xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   tokenAddr,
				Key:        balanceKey,
				Durability: xdr.ContractDataDurabilityPersistent,
			},
		})
		if err != nil {
			return nil, err
		}
		keyToToken[key] = token
		keys = append(keys, key)
		out[token] = 0
	}

	resp, err := rpc.GetLedgerEntries(ctx, protocol.GetLedgerEntriesRequest{Keys: keys})
	if err != nil {
		return nil, fmt.Errorf("getLedgerEntries: %w", err)
	}
	for _, e := range resp.Entries {
		token, ok := keyToToken[e.KeyXDR]
		if !ok {
			continue
		}
		var data xdr.LedgerEntryData
		if err := xdr.SafeUnmarshalBase64(e.DataXDR, &data); err != nil {
			return nil, fmt.Errorf("decode %s balance: %w", token, err)
		}
		cd, ok := data.GetContractData()
		if !ok {
			return nil, fmt.Errorf("%s balance entry is not contract data", token)
		}
		amount, err := sacBalanceAmount(cd.Val)
		if err != nil {
			return nil, fmt.Errorf("%s balance: %w", token, err)
		}
		out[token] = amount
	}
	return out, nil
}

// sacBalanceAmount extracts amount from a SAC balance value
// {amount: i128, authorized: bool, clawback: bool}.
func sacBalanceAmount(v xdr.ScVal) (int64, error) {
	m, ok := v.GetMap()
	if !ok || m == nil {
		return 0, fmt.Errorf("balance value is not a map")
	}
	for _, entry := range *m {
		if sym, ok := entry.Key.GetSym(); !ok || sym != "amount" {
			continue
		}
		parts, ok := entry.Val.GetI128()
		if !ok {
			return 0, fmt.Errorf("amount is not an i128")
		}
		if parts.Hi != 0 || int64(parts.Lo) < 0 {
			return 0, fmt.Errorf("amount does not fit in int64")
		}
		return int64(parts.Lo), nil
	}
	return 0, fmt.Errorf("balance value has no amount")
}

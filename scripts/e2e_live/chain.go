package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// chain reads and writes testnet directly: the depositor's payment never goes
// through latch-api, and the assertions check the ledger, not just what the
// services report.
type chain struct {
	horizon *horizonclient.Client
	rpc     *rpcclient.Client
}

func newChain(horizonURL, rpcURL string) *chain {
	return &chain{
		horizon: &horizonclient.Client{HorizonURL: horizonURL, HTTP: &http.Client{Timeout: 60 * time.Second}},
		rpc:     rpcclient.NewClient(rpcURL, &http.Client{Timeout: 30 * time.Second}),
	}
}

func (c *chain) requireTestnet(ctx context.Context) error {
	root, err := c.horizon.Root()
	if err != nil {
		return fmt.Errorf("horizon root: %w", err)
	}
	if root.NetworkPassphrase != network.TestNetworkPassphrase {
		return fmt.Errorf("horizon is on %q, not testnet", root.NetworkPassphrase)
	}
	n, err := c.rpc.GetNetwork(ctx)
	if err != nil {
		return fmt.Errorf("rpc getNetwork: %w", err)
	}
	if n.Passphrase != network.TestNetworkPassphrase {
		return fmt.Errorf("rpc is on %q, not testnet", n.Passphrase)
	}
	return nil
}

func (c *chain) xlmBalance(address string) (string, error) {
	acct, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		return "", err
	}
	return acct.GetNativeBalance()
}

// pay sends amount XLM from src to dest; memo may be nil for "no memo".
func (c *chain) pay(src *keypair.Full, dest, amount string, memo txnbuild.Memo) (string, error) {
	acct, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: src.Address()})
	if err != nil {
		return "", fmt.Errorf("load %s: %w", src.Address(), err)
	}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &acct,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{&txnbuild.Payment{
			Destination: dest,
			Amount:      amount,
			Asset:       txnbuild.NativeAsset{},
		}},
		Memo:          memo,
		BaseFee:       txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		return "", err
	}
	tx, err = tx.Sign(network.TestNetworkPassphrase, src)
	if err != nil {
		return "", err
	}
	res, err := c.horizon.SubmitTransaction(tx)
	if err != nil {
		return "", fmt.Errorf("submit payment: %w", describeHorizonErr(err))
	}
	return res.Hash, nil
}

// txSucceeded reports whether a transaction landed and succeeded.
func (c *chain) txSucceeded(hash string) (bool, error) {
	tx, err := c.horizon.TransactionDetail(hash)
	if err != nil {
		return false, err
	}
	return tx.Successful, nil
}

// findOutgoingPayment looks for a native payment of exactly amount leaving
// from, created at or after since. Deposit amounts are unique per run, so a
// match is the sweep of that deposit.
func (c *chain) findOutgoingPayment(from, amount string, since time.Time) (*operations.Payment, error) {
	page, err := c.horizon.Payments(horizonclient.OperationRequest{
		ForAccount: from,
		Order:      horizonclient.OrderDesc,
		Limit:      100,
	})
	if err != nil {
		return nil, err
	}
	for _, rec := range page.Embedded.Records {
		p, ok := rec.(operations.Payment)
		if !ok || p.From != from || p.Asset.Type != "native" || p.Amount != amount {
			continue
		}
		if p.LedgerCloseTime.Before(since) {
			continue
		}
		return &p, nil
	}
	return nil, nil
}

// sacBalance returns holder's native XLM balance (in stroops) as held by the
// native Stellar Asset Contract, which is where a C-address keeps XLM. It
// simulates balance(holder), so nothing is signed or submitted.
func (c *chain) sacBalance(ctx context.Context, source, holder string) (int64, error) {
	sacID, err := xdr.MustNewNativeAsset().ContractID(network.TestNetworkPassphrase)
	if err != nil {
		return 0, err
	}
	sac := xdr.ContractId(sacID)
	holderRaw, err := strkey.Decode(strkey.VersionByteContract, holder)
	if err != nil {
		return 0, fmt.Errorf("decode %s: %w", holder, err)
	}
	var holderID xdr.ContractId
	copy(holderID[:], holderRaw)
	holderAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &holderID}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		// Simulation ignores the sequence number, so no account lookup is needed.
		SourceAccount: &txnbuild.SimpleAccount{AccountID: source},
		Operations: []txnbuild.Operation{&txnbuild.InvokeHostFunction{HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &sac},
				FunctionName:    "balance",
				Args:            []xdr.ScVal{{Type: xdr.ScValTypeScvAddress, Address: &holderAddr}},
			},
		}}},
		BaseFee:       txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
	})
	if err != nil {
		return 0, err
	}
	b64, err := tx.Base64()
	if err != nil {
		return 0, err
	}
	resp, err := c.rpc.SimulateTransaction(ctx, protocol.SimulateTransactionRequest{Transaction: b64})
	if err != nil {
		return 0, err
	}
	if resp.Error != "" {
		return 0, fmt.Errorf("simulate balance: %s", resp.Error)
	}
	if len(resp.Results) == 0 || resp.Results[0].ReturnValueXDR == nil {
		return 0, fmt.Errorf("simulate balance: no result")
	}
	var val xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(*resp.Results[0].ReturnValueXDR, &val); err != nil {
		return 0, err
	}
	if val.Type != xdr.ScValTypeScvI128 || val.I128 == nil {
		return 0, fmt.Errorf("balance returned %v, want i128", val.Type)
	}
	if val.I128.Hi != 0 {
		return 0, fmt.Errorf("balance does not fit in int64")
	}
	return int64(val.I128.Lo), nil
}

func friendbot(address string) error {
	resp, err := http.Get("https://friendbot.stellar.org/?addr=" + url.QueryEscape(address))
	if err != nil {
		return fmt.Errorf("friendbot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("friendbot: HTTP %d: %s", resp.StatusCode, trim(raw))
	}
	return nil
}

// describeHorizonErr surfaces Horizon's result codes, which the default error
// string hides.
func describeHorizonErr(err error) error {
	if herr, ok := err.(*horizonclient.Error); ok {
		if codes, cerr := herr.ResultCodes(); cerr == nil {
			return fmt.Errorf("%w (tx=%s ops=%v)", err, codes.TransactionCode, codes.OperationCodes)
		}
	}
	return err
}

func txLink(hash string) string {
	return "https://stellar.expert/explorer/testnet/tx/" + hash
}

func contractLink(addr string) string {
	return addr + " (https://stellar.expert/explorer/testnet/contract/" + addr + ")"
}

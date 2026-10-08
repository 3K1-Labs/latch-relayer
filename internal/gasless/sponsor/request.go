// Package sponsor submits wallet transactions on behalf of latch-api: it
// validates what it is asked to pay for, enforces the sponsorship caps, and
// sends the transaction through a leased channel account with a fee-bump from
// the funder.
//
// latch-api stays the place that decides what a wallet transaction should be
// (one invoke-host-function op, allowed contracts, enforcing simulation). The
// checks here are defence in depth for the one thing this service owns: the
// funder's XLM.
package sponsor

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Mode is who reimburses the network fee.
type Mode string

const (
	// ModeSponsored: Latch pays and the user is charged nothing. Only for a
	// new wallet's setup transactions (see Policy).
	ModeSponsored Mode = "sponsored"
	// ModeForward: the user reimburses the fee in XLM or USDC through the
	// FeeForwarder contract. Not implemented yet.
	ModeForward Mode = "forward"
)

// Request is one submission from latch-api.
type Request struct {
	// RequestID makes the call idempotent: the same id returns the same
	// outcome and never pays twice.
	RequestID string `json:"request_id"`
	// Wallet is the C-address the transaction is for. Sponsorship caps are
	// counted against it.
	Wallet string `json:"wallet"`
	Mode   Mode   `json:"mode"`
	// Transaction is an unsigned TransactionEnvelope (base64) holding exactly
	// one InvokeHostFunction operation with its authorization entries already
	// signed. Its source account, sequence number, fee and Soroban data are
	// ignored: this service re-sources it onto a channel and re-simulates.
	Transaction string `json:"transaction"`
}

// Call is the validated content of a request: what will be invoked and with
// which authorizations.
type Call struct {
	Contract     string // C-address
	Function     string
	HostFunction xdr.HostFunction
	Auth         []xdr.SorobanAuthorizationEntry
}

// Validation errors. Each maps to one HTTP status in the API.
var (
	ErrInvalid          = errors.New("invalid request")
	ErrNotSponsorable   = errors.New("transaction is not eligible for sponsorship")
	ErrForwardNotBuilt  = errors.New("forward mode is not implemented yet")
	requestIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	maxTransactionBytes = 64 * 1024
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks req and extracts the call. relayerAccounts are this
// service's own addresses (executor, funder, channels): no authorization
// entry may claim to act for them.
func Validate(req Request, policy Policy, relayerAccounts map[string]bool) (Call, error) {
	if !requestIDPattern.MatchString(req.RequestID) {
		return Call{}, invalid("request_id must be 8-64 characters of [A-Za-z0-9_-]")
	}
	if !strkey.IsValidContractAddress(req.Wallet) {
		return Call{}, invalid("wallet must be a C-address")
	}
	switch req.Mode {
	case ModeSponsored:
	case ModeForward:
		return Call{}, ErrForwardNotBuilt
	default:
		return Call{}, invalid("mode must be %q or %q", ModeSponsored, ModeForward)
	}
	if req.Transaction == "" || len(req.Transaction) > maxTransactionBytes {
		return Call{}, invalid("transaction is required and must be at most %d bytes", maxTransactionBytes)
	}

	var env xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(req.Transaction, &env); err != nil {
		return Call{}, invalid("transaction is not a base64 TransactionEnvelope: %v", err)
	}
	if env.Type != xdr.EnvelopeTypeEnvelopeTypeTx || env.V1 == nil {
		return Call{}, invalid("transaction must be a v1 transaction envelope, not %s", env.Type)
	}
	ops := env.V1.Tx.Operations
	if len(ops) != 1 {
		return Call{}, invalid("transaction must have exactly one operation, has %d", len(ops))
	}
	invoke, ok := ops[0].Body.GetInvokeHostFunctionOp()
	if !ok {
		return Call{}, invalid("operation must be InvokeHostFunction, is %s", ops[0].Body.Type)
	}
	args, ok := invoke.HostFunction.GetInvokeContract()
	if !ok {
		return Call{}, invalid("host function must invoke a contract, is %s", invoke.HostFunction.Type)
	}
	contract, err := scAddressString(args.ContractAddress)
	if err != nil || !strkey.IsValidContractAddress(contract) {
		return Call{}, invalid("invoked address must be a contract")
	}

	for i, entry := range invoke.Auth {
		addr, err := authAddress(entry.Credentials)
		if err != nil {
			return Call{}, invalid("auth entry %d: %v", i, err)
		}
		if relayerAccounts[addr] {
			return Call{}, invalid("auth entry %d claims to act for a relayer account", i)
		}
	}

	call := Call{
		Contract:     contract,
		Function:     string(args.FunctionName),
		HostFunction: invoke.HostFunction,
		Auth:         invoke.Auth,
	}
	if !policy.Allows(call, req.Wallet) {
		return Call{}, fmt.Errorf("%w: %s.%s is not a sponsored setup call for wallet %s",
			ErrNotSponsorable, call.Contract, call.Function, req.Wallet)
	}
	return call, nil
}

// authAddress returns the address an entry authorizes for. Source-account
// credentials are refused: they authorize as whoever the transaction's
// source is, and here that is one of this service's channels.
func authAddress(creds xdr.SorobanCredentials) (string, error) {
	switch creds.Type {
	case xdr.SorobanCredentialsTypeSorobanCredentialsAddress:
		return scAddressString(creds.MustAddress().Address)
	case xdr.SorobanCredentialsTypeSorobanCredentialsAddressV2:
		return scAddressString(creds.MustAddressV2().Address)
	case xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount:
		return "", errors.New("source-account credentials are not allowed")
	default:
		return "", fmt.Errorf("unsupported credentials type %s", creds.Type)
	}
}

func scAddressString(a xdr.ScAddress) (string, error) {
	s, err := a.String()
	if err != nil {
		return "", fmt.Errorf("decode address: %w", err)
	}
	return s, nil
}

// WalletContract is the placeholder in a policy rule for "the wallet the
// request is for".
const WalletContract = "wallet"

// Rule is one sponsorable call: Function on Contract, where Contract is a
// C-address or WalletContract.
type Rule struct {
	Contract string
	Function string
}

// Policy is the set of calls Latch pays for: a new wallet's setup
// transactions only (deployment, setup-send-rules, setup-swap-rules).
type Policy struct{ Rules []Rule }

// ParsePolicy reads "CONTRACT:function,CONTRACT:function", where CONTRACT is
// a C-address or the word "wallet". Example:
//
//	CFACTORY...:create_account,wallet:add_context_rule,wallet:add_signer
func ParsePolicy(s string) (Policy, error) {
	var p Policy
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		contract, fn, ok := strings.Cut(part, ":")
		if !ok || fn == "" {
			return Policy{}, fmt.Errorf("sponsored call %q must be CONTRACT:function", part)
		}
		if contract != WalletContract && !strkey.IsValidContractAddress(contract) {
			return Policy{}, fmt.Errorf("sponsored call %q: contract must be a C-address or %q", part, WalletContract)
		}
		p.Rules = append(p.Rules, Rule{Contract: contract, Function: fn})
	}
	if len(p.Rules) == 0 {
		return Policy{}, errors.New("no sponsored calls configured")
	}
	return p, nil
}

// Allows reports whether call, made for wallet, is a sponsored setup call.
func (p Policy) Allows(call Call, wallet string) bool {
	for _, r := range p.Rules {
		if r.Function != call.Function {
			continue
		}
		if r.Contract == call.Contract || (r.Contract == WalletContract && call.Contract == wallet) {
			return true
		}
	}
	return false
}

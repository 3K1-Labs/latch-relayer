package sponsor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/signer"
)

// Forward mode: the user reimburses the network fee in XLM or USDC through
// the FeeForwarder contract. The wallet is a contract and can't pay its own
// fee, so the funder still pays the XLM; FeeForwarder collects it back from
// the user in the same transaction.
//
// FeeForwarder.forward(fee_token, fee_amount, max_fee_amount,
// expiration_ledger, target_contract, target_fn, target_args, user, relayer):
//   - the user authorizes (fee_token, max_fee_amount, expiration_ledger,
//     target_contract, target_fn, target_args), with the fee token's approve
//     (when the allowance is short) and the target call beneath it;
//   - the executor (relayer) authorizes all nine arguments, so this service
//     fills in fee_amount (≤ max_fee_amount) and its own address, then signs.

// forwardFn is FeeForwarder's entrypoint.
const forwardFn = "forward"

// Argument positions in forward().
const (
	argFeeToken = iota
	argFeeAmount
	argMaxFee
	argExpiration
	argTarget
	argTargetFn
	argTargetArgs
	argUser
	argRelayer
	forwardArgCount
)

// FeeToken is a token users may pay fees in. Both allowed tokens are SACs
// with 7 decimals, so one stroop of XLM is one unit of either.
type FeeToken struct {
	Contract string `json:"contract"` // SAC C-address
	Symbol   string `json:"symbol"`   // "XLM" or "USDC"
	Native   bool   `json:"native"`   // XLM's own SAC: fee units are stroops, no price needed
}

// PriceSource reports XLM's price in USD. USDC is treated as one dollar.
type PriceSource interface {
	XLMUSD(ctx context.Context) (float64, error)
}

// Forward configures forward mode.
type Forward struct {
	ForwarderID string
	// Executor holds FeeForwarder's executor role and signs the relayer's
	// authorization for every forward().
	Executor signer.Signer
	Tokens   map[string]FeeToken // by contract
	Prices   PriceSource
	// MarginBps is added on top of the network cost before converting:
	// covers resource drift between simulations and price movement.
	MarginBps int64
	// AuthLedgers is how long the executor's signature stays valid.
	AuthLedgers uint32
}

var (
	// ErrFeeTooLow: the user's signed max_fee_amount doesn't cover the
	// current network cost. Nothing was sent; quote again and re-sign.
	ErrFeeTooLow = errors.New("max_fee_amount does not cover the network fee")
	// ErrPriceUnavailable: no XLM price to convert a USDC fee with.
	ErrPriceUnavailable = errors.New("fee price unavailable")
)

// forwardCall is a validated forward() invocation.
type forwardCall struct {
	args     []xdr.ScVal
	token    FeeToken
	maxFee   int64
	user     string
	target   string
	targetFn string
}

// parseForward checks a call is forward() on the configured FeeForwarder
// for wallet, paid in an allowed token.
func (f *Forward) parseForward(call Call, wallet string) (forwardCall, error) {
	if call.Contract != f.ForwarderID || call.Function != forwardFn {
		return forwardCall{}, invalid("forward mode must invoke %s.%s", f.ForwarderID, forwardFn)
	}
	args := call.HostFunction.MustInvokeContract().Args
	if len(args) != forwardArgCount {
		return forwardCall{}, invalid("forward() takes %d arguments, got %d", forwardArgCount, len(args))
	}
	token, err := scAddr(args[argFeeToken])
	if err != nil {
		return forwardCall{}, invalid("fee_token: %v", err)
	}
	ft, ok := f.Tokens[token]
	if !ok {
		return forwardCall{}, invalid("fee_token %s is not an accepted fee token", token)
	}
	maxFee, err := scI128(args[argMaxFee])
	if err != nil || maxFee <= 0 {
		return forwardCall{}, invalid("max_fee_amount must be a positive i128")
	}
	if args[argExpiration].Type != xdr.ScValTypeScvU32 {
		return forwardCall{}, invalid("expiration_ledger must be a u32")
	}
	target, err := scAddr(args[argTarget])
	if err != nil {
		return forwardCall{}, invalid("target_contract: %v", err)
	}
	if target == f.ForwarderID {
		return forwardCall{}, invalid("target_contract must not be the FeeForwarder")
	}
	if args[argTargetFn].Type != xdr.ScValTypeScvSymbol {
		return forwardCall{}, invalid("target_fn must be a symbol")
	}
	user, err := scAddr(args[argUser])
	if err != nil || user != wallet {
		return forwardCall{}, invalid("user must be the request's wallet")
	}
	// fee_amount and relayer are this service's to set; any value is replaced.
	return forwardCall{
		args: args, token: ft, maxFee: maxFee, user: user,
		target: target, targetFn: string(*args[argTargetFn].Sym),
	}, nil
}

// withFee returns the host function with fee_amount and relayer filled in.
func (f *Forward) withFee(call Call, feeAmount int64) (xdr.HostFunction, error) {
	inv := call.HostFunction.MustInvokeContract()
	args := append([]xdr.ScVal(nil), inv.Args...)
	args[argFeeAmount] = i128(feeAmount)
	relayer, err := accountScVal(f.Executor.Address())
	if err != nil {
		return xdr.HostFunction{}, err
	}
	args[argRelayer] = relayer
	inv.Args = args
	return xdr.HostFunction{Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract, InvokeContract: &inv}, nil
}

// FeeInToken converts a cost in stroops to the token's units, adding the
// margin and rounding up.
func (f *Forward) FeeInToken(ctx context.Context, token FeeToken, stroops int64) (int64, error) {
	withMargin := new(big.Rat).SetFrac64(stroops*(10_000+f.MarginBps), 10_000)
	if !token.Native {
		price, err := f.Prices.XLMUSD(ctx)
		if err != nil || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return 0, fmt.Errorf("%w: %v", ErrPriceUnavailable, err)
		}
		// The shortest decimal form, not SetFloat64: 0.2 as a float64 is a
		// hair above 0.2, which would round every fee up by a unit.
		p, ok := new(big.Rat).SetString(strconv.FormatFloat(price, 'f', -1, 64))
		if !ok {
			return 0, fmt.Errorf("%w: bad price %v", ErrPriceUnavailable, price)
		}
		withMargin.Mul(withMargin, p)
	}
	// Ceil: never charge less than the cost because of rounding.
	q, r := new(big.Int).QuoRem(withMargin.Num(), withMargin.Denom(), new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("fee overflows: %s", q)
	}
	return max(q.Int64(), 1), nil
}

// signExecutorEntry signs a recorded executor authorization entry, valid
// until expirationLedger. The signature is the account format soroban
// expects for a G-address: Vec[Map{public_key: Bytes, signature: Bytes}].
func signExecutorEntry(ctx context.Context, s signer.Signer, passphrase string, entry xdr.SorobanAuthorizationEntry, expirationLedger uint32) (xdr.SorobanAuthorizationEntry, error) {
	if entry.Credentials.Type != xdr.SorobanCredentialsTypeSorobanCredentialsAddress || entry.Credentials.Address == nil {
		return xdr.SorobanAuthorizationEntry{}, errors.New("executor entry must use address credentials")
	}
	creds := *entry.Credentials.Address
	creds.SignatureExpirationLedger = xdr.Uint32(expirationLedger)

	preimage := xdr.HashIdPreimage{
		Type: xdr.EnvelopeTypeEnvelopeTypeSorobanAuthorization,
		SorobanAuthorization: &xdr.HashIdPreimageSorobanAuthorization{
			NetworkId:                 xdr.Hash(sha256.Sum256([]byte(passphrase))),
			Nonce:                     creds.Nonce,
			SignatureExpirationLedger: creds.SignatureExpirationLedger,
			Invocation:                entry.RootInvocation,
		},
	}
	raw, err := preimage.MarshalBinary()
	if err != nil {
		return xdr.SorobanAuthorizationEntry{}, fmt.Errorf("encode auth preimage: %w", err)
	}
	sig, err := s.Sign(ctx, sha256.Sum256(raw))
	if err != nil {
		return xdr.SorobanAuthorizationEntry{}, fmt.Errorf("sign executor auth: %w", err)
	}
	pub, err := strkey.Decode(strkey.VersionByteAccountID, s.Address())
	if err != nil {
		return xdr.SorobanAuthorizationEntry{}, fmt.Errorf("executor public key: %w", err)
	}
	pubSym, sigSym := xdr.ScSymbol("public_key"), xdr.ScSymbol("signature")
	pubBytes, sigBytes := xdr.ScBytes(pub), xdr.ScBytes(sig)
	m := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &pubSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &pubBytes}},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sigSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &sigBytes}},
	}
	mp := &m
	vec := xdr.ScVec{{Type: xdr.ScValTypeScvMap, Map: &mp}}
	vp := &vec
	creds.Signature = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}

	signed := entry
	signed.Credentials = xdr.SorobanCredentials{Type: xdr.SorobanCredentialsTypeSorobanCredentialsAddress, Address: &creds}
	return signed, nil
}

// setRootFeeAmount rewrites fee_amount in an executor entry recorded with a
// placeholder, so the signature covers the amount actually charged.
func setRootFeeAmount(entry xdr.SorobanAuthorizationEntry, feeAmount int64) (xdr.SorobanAuthorizationEntry, error) {
	fn := entry.RootInvocation.Function
	if fn.Type != xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn || fn.ContractFn == nil ||
		len(fn.ContractFn.Args) != forwardArgCount {
		return xdr.SorobanAuthorizationEntry{}, errors.New("executor entry is not a forward() authorization")
	}
	inv := *fn.ContractFn
	args := append([]xdr.ScVal(nil), inv.Args...)
	args[argFeeAmount] = i128(feeAmount)
	inv.Args = args
	out := entry
	out.RootInvocation.Function.ContractFn = &inv
	return out, nil
}

// ── ScVal helpers ────────────────────────────────────────────────────────────

func scAddr(v xdr.ScVal) (string, error) {
	if v.Type != xdr.ScValTypeScvAddress || v.Address == nil {
		return "", errors.New("not an address")
	}
	return scAddressString(*v.Address)
}

func scI128(v xdr.ScVal) (int64, error) {
	if v.Type != xdr.ScValTypeScvI128 || v.I128 == nil {
		return 0, errors.New("not an i128")
	}
	if v.I128.Hi != 0 || uint64(v.I128.Lo) > math.MaxInt64 {
		return 0, errors.New("i128 out of range")
	}
	return int64(v.I128.Lo), nil
}

func i128(n int64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(n)}}
}

func accountScVal(g string) (xdr.ScVal, error) {
	aid, err := xdr.AddressToAccountId(g)
	if err != nil {
		return xdr.ScVal{}, fmt.Errorf("account %s: %w", g, err)
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}}, nil
}

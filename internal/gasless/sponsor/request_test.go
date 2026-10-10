package sponsor

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func contractAddr(t *testing.T, b byte) (string, xdr.ScAddress) {
	t.Helper()
	var id xdr.ContractId
	for i := range id {
		id[i] = b
	}
	s, err := strkey.Encode(strkey.VersionByteContract, id[:])
	if err != nil {
		t.Fatal(err)
	}
	return s, xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
}

func accountScAddress(t *testing.T, g string) xdr.ScAddress {
	t.Helper()
	aid := xdr.MustAddress(g)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
}

func addressAuth(addr xdr.ScAddress, invoke xdr.InvokeContractArgs) xdr.SorobanAuthorizationEntry {
	return xdr.SorobanAuthorizationEntry{
		Credentials: xdr.SorobanCredentials{
			Type: xdr.SorobanCredentialsTypeSorobanCredentialsAddress,
			Address: &xdr.SorobanAddressCredentials{
				Address:   addr,
				Nonce:     1,
				Signature: xdr.ScVal{Type: xdr.ScValTypeScvVoid},
			},
		},
		RootInvocation: xdr.SorobanAuthorizedInvocation{Function: xdr.SorobanAuthorizedFunction{
			Type:       xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
			ContractFn: &invoke,
		}},
	}
}

func invokeArgs(contract xdr.ScAddress, fn string) xdr.InvokeContractArgs {
	return xdr.InvokeContractArgs{ContractAddress: contract, FunctionName: xdr.ScSymbol(fn), Args: []xdr.ScVal{}}
}

// envelopeB64 builds a submit-shaped envelope: any source, one or more ops.
func envelopeB64(t *testing.T, ops ...xdr.Operation) string {
	t.Helper()
	var src xdr.MuxedAccount
	if err := src.SetAddress(keypair.MustRandom().Address()); err != nil {
		t.Fatal(err)
	}
	env := xdr.TransactionEnvelope{Type: xdr.EnvelopeTypeEnvelopeTypeTx, V1: &xdr.TransactionV1Envelope{Tx: xdr.Transaction{
		SourceAccount: src, Fee: 100, SeqNum: 1, Operations: ops,
	}}}
	b64, err := xdr.MarshalBase64(env)
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

func invokeOp(invoke xdr.InvokeContractArgs, auth ...xdr.SorobanAuthorizationEntry) xdr.Operation {
	return xdr.Operation{Body: xdr.OperationBody{
		Type: xdr.OperationTypeInvokeHostFunction,
		InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
			HostFunction: xdr.HostFunction{Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract, InvokeContract: &invoke},
			Auth:         auth,
		},
	}}
}

func TestParsePolicy(t *testing.T) {
	factory, _ := contractAddr(t, 7)
	p, err := ParsePolicy(factory + ":create_account, wallet:add_context_rule ,wallet:add_signer")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 3 || p.Rules[0].Contract != factory || p.Rules[1].Function != "add_context_rule" {
		t.Fatalf("rules = %+v", p.Rules)
	}
	for name, in := range map[string]string{
		"empty":         " , ",
		"no function":   "wallet:",
		"no separator":  "wallet",
		"G as contract": keypair.MustRandom().Address() + ":create_account",
	} {
		if _, err := ParsePolicy(in); err == nil {
			t.Errorf("%s: want error for %q", name, in)
		}
	}
}

func TestValidate(t *testing.T) {
	wallet, walletSc := contractAddr(t, 1)
	factory, factorySc := contractAddr(t, 2)
	_, otherSc := contractAddr(t, 3)
	relayer := keypair.MustRandom().Address()
	policy, err := ParsePolicy(factory + ":create_account,wallet:add_context_rule")
	if err != nil {
		t.Fatal(err)
	}
	relayers := map[string]bool{relayer: true}

	addRule := invokeArgs(walletSc, "add_context_rule")
	userAuth := addressAuth(walletSc, addRule)
	valid := Request{RequestID: "req_0001", Wallet: wallet, Mode: ModeSponsored, Transaction: envelopeB64(t, invokeOp(addRule, userAuth))}

	call, err := Validate(valid, policy, relayers, false)
	if err != nil {
		t.Fatalf("valid wallet setup call: %v", err)
	}
	if call.Contract != wallet || call.Function != "add_context_rule" || len(call.Auth) != 1 {
		t.Fatalf("call = %+v", call)
	}

	deploy := valid
	deploy.Transaction = envelopeB64(t, invokeOp(invokeArgs(factorySc, "create_account")))
	if _, err := Validate(deploy, policy, relayers, false); err != nil {
		t.Fatalf("factory deploy (no auth): %v", err)
	}

	sourceAuth := userAuth
	sourceAuth.Credentials = xdr.SorobanCredentials{Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount}

	cases := map[string]struct {
		mutate func(r *Request)
		want   error
	}{
		"short request id": {func(r *Request) { r.RequestID = "x" }, ErrInvalid},
		"wallet not C":     {func(r *Request) { r.Wallet = relayer }, ErrInvalid},
		"unknown mode":     {func(r *Request) { r.Mode = "free" }, ErrInvalid},
		"forward mode":     {func(r *Request) { r.Mode = ModeForward }, ErrForwardNotBuilt},
		"not base64":       {func(r *Request) { r.Transaction = "!!" }, ErrInvalid},
		"two operations":   {func(r *Request) { r.Transaction = envelopeB64(t, invokeOp(addRule), invokeOp(addRule)) }, ErrInvalid},
		"source-account auth": {func(r *Request) {
			r.Transaction = envelopeB64(t, invokeOp(addRule, sourceAuth))
		}, ErrInvalid},
		"auth for a relayer account": {func(r *Request) {
			r.Transaction = envelopeB64(t, invokeOp(addRule, addressAuth(accountScAddress(t, relayer), addRule)))
		}, ErrInvalid},
		"not on the allowlist": {func(r *Request) {
			r.Transaction = envelopeB64(t, invokeOp(invokeArgs(walletSc, "execute")))
		}, ErrNotSponsorable},
		"wallet rule on another contract": {func(r *Request) {
			r.Transaction = envelopeB64(t, invokeOp(invokeArgs(otherSc, "add_context_rule")))
		}, ErrNotSponsorable},
		"factory function not allowed": {func(r *Request) {
			r.Transaction = envelopeB64(t, invokeOp(invokeArgs(factorySc, "upgrade")))
		}, ErrNotSponsorable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			if _, err := Validate(req, policy, relayers, false); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

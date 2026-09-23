package chain

import (
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// The SDK's xdr package has no convenience constructors for Soroban values;
// these mirror the ones in latch-api (internal/service/webapp/soroban_scval.go).

func ScSymbol(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

// ScAddressFromString builds an ScAddress from a G… account or C… contract.
func ScAddressFromString(address string) (xdr.ScAddress, error) {
	switch {
	case strings.HasPrefix(address, "G"):
		aid, err := xdr.AddressToAccountId(address)
		if err != nil {
			return xdr.ScAddress{}, fmt.Errorf("decode account address %q: %w", address, err)
		}
		return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}, nil
	case strings.HasPrefix(address, "C"):
		id, err := ContractID(address)
		if err != nil {
			return xdr.ScAddress{}, err
		}
		return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}, nil
	default:
		return xdr.ScAddress{}, fmt.Errorf("unsupported address %q", address)
	}
}

// ScAddressVal wraps an address as an ScVal argument.
func ScAddressVal(address string) (xdr.ScVal, error) {
	addr, err := ScAddressFromString(address)
	if err != nil {
		return xdr.ScVal{}, err
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}, nil
}

func ContractID(address string) (xdr.ContractId, error) {
	var id xdr.ContractId
	raw, err := strkey.Decode(strkey.VersionByteContract, address)
	if err != nil {
		return id, fmt.Errorf("decode contract address %q: %w", address, err)
	}
	copy(id[:], raw)
	return id, nil
}

// InvokeContract builds the host function for contract.fn(args...).
func InvokeContract(contract, fn string, args ...xdr.ScVal) (xdr.HostFunction, error) {
	id, err := ContractID(contract)
	if err != nil {
		return xdr.HostFunction{}, err
	}
	return xdr.HostFunction{
		Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		InvokeContract: &xdr.InvokeContractArgs{
			ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
			FunctionName:    xdr.ScSymbol(fn),
			Args:            args,
		},
	}, nil
}

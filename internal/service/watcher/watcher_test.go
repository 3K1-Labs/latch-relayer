package watcher

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	pool  = "GB3AETG6Q5SYNM36TPHULYXPP364EC77YJJBDMNKDA5CF4QYP3XHJ6I5"
	other = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
	usdc  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// A path payment into the pool used to be skipped with its cursor saved: the
// money landed and nothing ever credited or swept it (#52). Decoded from
// Horizon's own JSON so the destination fields are the ones read.
func TestPaymentTo_PathPaymentsCreditTheDestinationSide(t *testing.T) {
	cases := map[xdr.OperationType]string{
		xdr.OperationTypePathPaymentStrictReceive: `"source_max": "50.0000000"`,
		xdr.OperationTypePathPaymentStrictSend:    `"destination_min": "9.0000000"`,
	}
	for typ, extra := range cases {
		raw := `{
			"id": "12884905985", "type_i": ` + strconv.Itoa(int(typ)) + `,
			"transaction_hash": "abc",
			"from": "` + other + `", "to": "` + pool + `",
			"asset_type": "credit_alphanum4", "asset_code": "USDC", "asset_issuer": "` + usdc + `",
			"amount": "10.0000000",
			"source_asset_type": "native", "source_amount": "42.0000000",
			` + extra + `
		}`
		op, err := operations.UnmarshalOperation(int32(typ), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		p, ok := paymentTo(op, pool)
		if !ok {
			t.Fatalf("%s into the pool not recognised as a payment", typ)
		}
		if p.Amount != "10.0000000" || assetID(p) != "USDC:"+usdc {
			t.Errorf("%s credited %s %s, want the destination side 10.0000000 USDC", typ, p.Amount, assetID(p))
		}
	}
}

func TestPaymentTo_IgnoresPaymentsElsewhere(t *testing.T) {
	out := operations.Payment{From: pool, To: other, Amount: "1"}
	if _, ok := paymentTo(out, pool); ok {
		t.Error("a payment out of the pool (a sweep) was treated as a deposit")
	}
	if _, ok := paymentTo(operations.CreateAccount{}, pool); ok {
		t.Error("a non-payment was treated as a deposit")
	}
}

func TestUnhandledCredit(t *testing.T) {
	cases := []struct {
		name string
		op   operations.Operation
		want string
	}{
		{"merge into pool", operations.AccountMerge{Account: other, Into: pool}, "account_merge"},
		{"merge elsewhere", operations.AccountMerge{Account: pool, Into: other}, ""},
		{"contract transfer to pool", operations.InvokeHostFunction{AssetBalanceChanges: []operations.AssetContractBalanceChange{
			{Type: "transfer", From: other, To: pool, Amount: "1"}}}, "contract_transfer"},
		// Our own forward: a SAC transfer out of the pool.
		{"forward out of pool", operations.InvokeHostFunction{AssetBalanceChanges: []operations.AssetContractBalanceChange{
			{Type: "transfer", From: pool, To: "CCOX4AG3XESDAZC7L27AMQZ6KKMUWEU2KCHFXJ2PXNAXMDUCL225MN2P", Amount: "1"}}}, ""},
		{"unrelated", operations.CreateAccount{}, ""},
	}
	for _, c := range cases {
		if got := unhandledCredit(c.op, pool); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func payment(memoType, memo, toMuxed string, toMuxedID uint64) operations.Payment {
	p := operations.Payment{To: pool, ToMuxed: toMuxed, ToMuxedID: toMuxedID}
	p.Transaction = &hProtocol.Transaction{MemoType: memoType, Memo: memo}
	return p
}

// An M-address carries the tag in the destination itself. Before #52 a
// memo-less payment to M…(pool, id) was swept despite naming its intent.
func TestRoutingID(t *testing.T) {
	const m = "MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUAAAAAAAAAAAACJUQ"
	cases := []struct {
		name     string
		p        operations.Payment
		want     uint64
		conflict bool
		fails    bool
	}{
		{"memo id", payment("id", "42", "", 0), 42, false, false},
		{"numeric text memo", payment("text", "42", "", 0), 42, false, false},
		{"no tag", payment("none", "", "", 0), 0, false, true},
		{"muxed, no memo", payment("none", "", m, 7), 7, false, false},
		{"muxed and matching memo", payment("id", "7", m, 7), 7, false, false},
		{"muxed and exchange text reference", payment("text", "order-991", m, 7), 7, false, false},
		{"muxed id zero is still a tag", payment("none", "", m, 0), 0, false, false},
		{"muxed and conflicting memo", payment("id", "8", m, 7), 0, true, true},
	}
	for _, c := range cases {
		got, err := routingID(c.p)
		if (err != nil) != c.fails {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if c.conflict && !errors.Is(err, errTagConflict) {
			t.Errorf("%s: err = %v, want a tag conflict", c.name, err)
		}
		if err == nil && got != c.want {
			t.Errorf("%s: memo_id = %d, want %d", c.name, got, c.want)
		}
	}
}

// Keys must stay stable for existing rows (single-operation transactions keep
// the bare hash) and be distinct for every payment in a batched transaction.
func TestDepositKey(t *testing.T) {
	const h = "abc"
	const txTOID = int64(12884905984) // ledger 3, tx 1
	op := func(i int64) string { return strconv.FormatInt(txTOID+i, 10) }

	if k := depositKey(h, op(1), 1); k != h {
		t.Errorf("single-operation key = %q, want the bare hash", k)
	}
	first, second := depositKey(h, op(1), 2), depositKey(h, op(2), 2)
	if first == second || first == h || second == h {
		t.Errorf("batched keys %q, %q must be distinct from each other and from a bare hash", first, second)
	}
	if depositKey(h, op(2), 2) != second {
		t.Error("key not deterministic: a replayed event would be credited twice")
	}
	if k := depositKey(h, "not-a-number", 3); !strings.HasPrefix(k, h+":") {
		t.Errorf("unparseable op id key = %q, want it kept distinct from the bare hash", k)
	}
}

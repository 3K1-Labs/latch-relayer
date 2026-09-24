package chain

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
)

// Testnet deployment (latch-contracts docs/BUILD.md): the FeeForwarder and the
// executor address granted the role at deploy time.
const (
	testnetForwarder = "CB6KFDFN7CXIOSBOEXABOX6KPRK4X6VMPSDWCGYP564JXGN2LIH2QPWI"
	testnetExecutor  = "GBLDLFA2Y3RXGL3LZPFTZYDCAE5OZRUVDLBRWAZGA7ZRWT7BGSA7IHMT"
)

// Runs against the real testnet contract; opt in with GASLESS_TESTNET=1.
// Proves our has_role call encoding and Option<u32> decoding agree with the
// deployed contract, which the unit tests (fake RPC) can't.
func TestTestnetFeeForwarderRoles(t *testing.T) {
	if os.Getenv("GASLESS_TESTNET") != "1" {
		t.Skip("set GASLESS_TESTNET=1 to run against Stellar testnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rpc := rpcclient.NewClient("https://soroban-testnet.stellar.org", &http.Client{Timeout: 30 * time.Second})
	defer rpc.Close()

	net, err := rpc.GetNetwork(ctx)
	if err != nil || net.Passphrase != network.TestNetworkPassphrase {
		t.Fatalf("getNetwork: %+v %v", net, err)
	}

	// A throwaway, faucet-funded account to act as the simulation source.
	source := keypair.MustRandom().Address()
	resp, err := http.Get(fmt.Sprintf("%s?addr=%s", net.FriendbotURL, source))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("friendbot: %v %v", resp, err)
	}
	resp.Body.Close()

	accts, err := Accounts(ctx, rpc, []string{source, keypair.MustRandom().Address()})
	if err != nil {
		t.Fatal(err)
	}
	if a := accts[source]; !a.Exists || a.BalanceStroops != 10_000*10_000_000 {
		t.Fatalf("friendbot account: %+v", a)
	}

	granted, err := HasRole(ctx, rpc, network.TestNetworkPassphrase, source, testnetForwarder, testnetExecutor, "executor")
	if err != nil || !granted {
		t.Fatalf("deployed executor has_role = %v, %v; want true", granted, err)
	}
	random, err := HasRole(ctx, rpc, network.TestNetworkPassphrase, source, testnetForwarder, keypair.MustRandom().Address(), "executor")
	if err != nil || random {
		t.Fatalf("random address has_role = %v, %v; want false", random, err)
	}
}

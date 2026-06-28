package main

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/keypair"
)

func main() {
	hot, _ := keypair.Random()
	depositor, _ := keypair.Random()

	fmt.Println("=== HOT / POOLED ACCOUNT ===")
	fmt.Println("Public key: ", hot.Address())
	fmt.Println("Secret key: ", hot.Seed())

	fmt.Println("\n=== DEPOSITOR (TEST ACCOUNT) ===")
	fmt.Println("Public key: ", depositor.Address())
	fmt.Println("Secret key: ", depositor.Seed())
}

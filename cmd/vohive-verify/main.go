// vohive-verify is the minimal, statically linked signature verifier used by
// the bootstrap installer on hosts that do not already provide minisign.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/zanescope/vohive/internal/updater"
)

func main() {
	publicKeys := flag.String("public-keys", "", "semicolon-separated minisign public keys")
	messagePath := flag.String("file", "", "file whose signature will be checked")
	signaturePath := flag.String("signature", "", "minisign signature file")
	sha256Value := flag.String("sha256", "", "optional expected SHA-256 of -file")
	flag.Parse()
	if *publicKeys == "" || *messagePath == "" || *signaturePath == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: vohive-verify -public-keys KEY[;NEXT] -file PATH -signature PATH [-sha256 HEX]")
		os.Exit(2)
	}
	verifier, err := updater.NewMinisignVerifier(*publicKeys)
	if err != nil {
		fail(err)
	}
	message, err := os.ReadFile(*messagePath)
	if err != nil {
		fail(err)
	}
	signature, err := os.ReadFile(*signaturePath)
	if err != nil {
		fail(err)
	}
	if err := verifier.Verify(message, signature); err != nil {
		fail(err)
	}
	if *sha256Value != "" {
		if err := updater.VerifyFileSHA256(*messagePath, *sha256Value); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "verification failed: %v\n", err)
	os.Exit(1)
}

// updsign creates and uses the ed25519 key that signs Deej Mixer releases.
//
//	go run ./tools/updsign keygen          -> prints PRIVATE (keep secret) and PUBLIC keys (base64)
//	UPDATE_SIGNING_KEY=<private> go run ./tools/updsign sign dist/SHA256SUMS.txt
//	                                       -> writes dist/SHA256SUMS.txt.sig
//	go run ./tools/updsign verify <public> dist/SHA256SUMS.txt
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

func main() {
	if len(os.Args) < 2 {
		die("usage: updsign keygen | sign <file> | verify <publickey> <file>")
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			die("%v", err)
		}
		fmt.Println("PRIVATE (GitHub secret UPDATE_SIGNING_KEY):", base64.StdEncoding.EncodeToString(priv))
		fmt.Println("PUBLIC  (GitHub variable UPDATE_PUBKEY):   ", base64.StdEncoding.EncodeToString(pub))
	case "sign":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("UPDATE_SIGNING_KEY")))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			die("UPDATE_SIGNING_KEY is missing or not a base64 ed25519 private key")
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			die("%v", err)
		}
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(raw), data))
		if err := os.WriteFile(os.Args[2]+".sig", []byte(sig+"\n"), 0o644); err != nil {
			die("%v", err)
		}
		fmt.Println("signed", os.Args[2])
	case "verify":
		pub, _ := base64.StdEncoding.DecodeString(os.Args[2])
		data, _ := os.ReadFile(os.Args[3])
		sigb, _ := os.ReadFile(os.Args[3] + ".sig")
		sig, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigb)))
		if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, data, sig) {
			die("INVALID signature")
		}
		fmt.Println("valid signature")
	default:
		die("unknown command %q", os.Args[1])
	}
}

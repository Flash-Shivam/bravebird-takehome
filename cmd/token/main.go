// token mints a demo JWT from the JWT_SECRET env var.
// Usage: JWT_SECRET=... go run ./cmd/token -sub alice [-ttl 24h]
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/shivamjadhav/bravebird-takehome/internal/auth"
)

func main() {
	sub := flag.String("sub", "", "user id (sub claim)")
	ttl := flag.Duration("ttl", 24*time.Hour, "token lifetime")
	flag.Parse()
	secret := os.Getenv("JWT_SECRET")
	if *sub == "" || secret == "" {
		fmt.Fprintln(os.Stderr, "usage: JWT_SECRET=... token -sub <user> [-ttl 24h]")
		os.Exit(1)
	}
	fmt.Println(auth.Mint([]byte(secret), *sub, *ttl))
}

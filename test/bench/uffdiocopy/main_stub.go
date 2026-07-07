//go:build !linux

// Non-linux stub so "go build ./..." works on development hosts (e.g. darwin).
// The benchmark itself depends on userfaultfd(2), which is linux-only.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "uffdiocopy requires linux")
	os.Exit(1)
}

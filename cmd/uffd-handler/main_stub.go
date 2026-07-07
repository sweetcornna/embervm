//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "uffd-handler requires linux (userfaultfd)")
	os.Exit(1)
}

// Command crew-go holds the Go ports of crew subcommands.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "crew-go: usage: crew-go roster [crew] | sessions <branch> [--crew ID]")
	os.Exit(64)
}

// preflight validates a PageDocument file using the trusted component registry.
package main

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pagegraph"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: preflight <page.json>")
		os.Exit(2)
	}
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := pagegraph.ValidateJSON(b); err != nil {
		fmt.Fprintln(os.Stderr, "INVALID:", err)
		os.Exit(1)
	}
	fmt.Println("VALID: PageDocument conforms to the bundled V1 registry")
}

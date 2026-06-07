// Command cerber is a trust-first, self-contained AI provider proxy.
// See CLAUDE.md for the design principles and AUDIT.md for the upstream audit.
package main

import (
	"fmt"

	"cerber/internal/version"
)

func main() {
	fmt.Printf("cerber %s\n", version.String())
}

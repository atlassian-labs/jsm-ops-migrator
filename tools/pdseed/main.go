// Command pdseed seeds the PagerDuty test account. It is the only code in this
// repository allowed to write to PagerDuty, which is why it is a separate
// module that the CLI never imports.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pdseed: not implemented yet")
	os.Exit(1)
}

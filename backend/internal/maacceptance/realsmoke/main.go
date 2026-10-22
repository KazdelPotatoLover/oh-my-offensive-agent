// Command realsmoke is the explicit real-model entry. It does not call a
// provider unless every required variable is set, and this acceptance commit
// still refuses to send that call.
package main

import (
	"fmt"
	"os"
)

func main() {
	required := []string{"MA_REAL_SMOKE", "MA_REAL_MODEL", "MA_REAL_TARGET", "MA_REAL_BUDGET"}
	missing := make([]string, 0)
	for _, key := range required {
		if os.Getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 || os.Getenv("MA_REAL_SMOKE") != "1" {
		fmt.Fprintln(os.Stderr, "real smoke refused: set MA_REAL_SMOKE=1 plus MA_REAL_MODEL, MA_REAL_TARGET, and MA_REAL_BUDGET")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "real smoke not sent: this acceptance commit does not call a commercial model")
	os.Exit(3)
}

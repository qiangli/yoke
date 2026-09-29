// Command yoke-seedfit fits the band ladder's seed coding ability θ from the
// public benchmark matrix. See pkg/ladder/seedfit.
package main

import (
	"os"

	"github.com/qiangli/yoke/pkg/ladder/seedfit"
)

func main() {
	os.Exit(seedfit.Main(os.Args[1:], os.Stdout, os.Stderr))
}

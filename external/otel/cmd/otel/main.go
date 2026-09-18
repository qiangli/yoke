package main

import (
	"fmt"
	"os"

	"github.com/qiangli/yoke/external/otel/otelcli"
)

func main() {
	if err := otelcli.NewCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "otel:", err)
		os.Exit(1)
	}
}

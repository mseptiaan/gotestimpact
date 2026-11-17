// Command gotestimpact runs only the Go tests affected by your changes.
package main

import (
	"os"

	"github.com/mseptiaan/gotestimpact/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

package main

import (
	"os"

	"lexinet/internal/cli"
)

var version = "0.1.0-dev"

func main() {
	os.Exit(cli.Execute(version))
}

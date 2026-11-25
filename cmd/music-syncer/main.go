package main

import (
	"fmt"
	"os"

	"github.com/zxbass/music-syncer/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

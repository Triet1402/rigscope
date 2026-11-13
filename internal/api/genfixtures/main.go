// Command genfixtures writes Go-encoded browser API fixtures used by the
// TypeScript contract test. It is run by `go generate ./internal/api`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Triet1402/rigscope/internal/api/fixturesgen"
)

func main() {
	src := flag.String("src", "../../testdata/api", "captured fixture root")
	dst := flag.String("dst", "../../testdata/api/reencoded", "output directory")
	flag.Parse()
	if err := fixturesgen.Generate(*src, *dst); err != nil {
		fmt.Fprintf(os.Stderr, "generate api fixtures: %v\n", err)
		os.Exit(1)
	}
}

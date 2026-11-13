package cli

import (
	"flag"
	"io"
)

// newFlagSet returns a flag set that reports errors without dumping the full
// command usage on every typo.
func newFlagSet(name string, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {}
	return flags
}

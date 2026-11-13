// Command rigscope is the single Rigscope binary: CLI/installer, hub and
// collector. main only dispatches subcommands; behaviour lives in internal/.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	legal "github.com/Triet1402/rigscope"
	"github.com/Triet1402/rigscope/internal/cli"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		cli.Usage(stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "rigscope", version.Version)
		return 0
	case "license":
		fmt.Fprintln(stdout, "Rigscope — Copyright (c) 2026 Triet (@Triet1402). Licensed under AGPL-3.0-only.")
		fmt.Fprint(stdout, legal.License)
		return 0
	case "help", "--help", "-h":
		cli.Usage(stdout)
		return 0
	case "internal":
		// Hidden self-exec entry points. `internal pci-read` maps one PCI BAR0
		// batch in a throwaway process so a wedged device cannot stall or kill
		// the collector. Deliberately absent from usage.
		if len(args) == 2 && args[1] == gpu.HelperCommand {
			return gpu.RunPCIReadHelper(os.Stdin, stdout, stderr)
		}
		fmt.Fprintf(stderr, "rigscope: unknown internal command %q\n", args[1:])
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.Run(ctx, args, cli.DefaultDeps(stdout, stderr))
}

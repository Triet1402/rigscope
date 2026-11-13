package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// errCancelled is returned when the operator answers "no" to a stop prompt.
// Nothing has been stopped or changed at that point.
var errCancelled = errors.New("cancelled; nothing was stopped or changed")

// errNoTerminal means a stop prompt was needed but there is no terminal to
// ask on and --yes was not given.
var errNoTerminal = errors.New("no terminal to confirm on")

// ANSI styles used by the CLI. They are applied only when the destination is
// a terminal and NO_COLOR is unset, so redirected output and tests stay plain.
const (
	styleBold   = "1"
	styleDim    = "2"
	styleRed    = "31"
	styleGreen  = "32"
	styleYellow = "33"
	styleCyan   = "36"
)

// isTerminal reports whether w is a terminal. /dev/null is a character device
// too, so the check asks for terminal attributes instead of the file mode.
func isTerminal(w any) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

// colorAllowed applies the NO_COLOR convention (https://no-color.org) and a
// dumb terminal before looking at the stream itself.
func colorAllowed(getenv func(string) string, w any) bool {
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(w)
}

// paint wraps s in an ANSI style when color is enabled for that stream.
func paint(enabled bool, style, s string) string {
	if !enabled || s == "" {
		return s
	}
	return "\x1b[" + style + "m" + s + "\x1b[0m"
}

// out styles text written to Stdout; errStyle styles text written to Stderr.
func (d Deps) out(style, s string) string { return paint(d.ColorStdout, style, s) }
func (d Deps) errStyle(style, s string) string {
	return paint(d.ColorStderr, style, s)
}

// warnf prints a highlighted warning line on w (Stdout or Stderr).
func (d Deps) warnf(w io.Writer, format string, args ...any) {
	enabled := d.ColorStdout
	if w == d.Stderr {
		enabled = d.ColorStderr
	}
	fmt.Fprintf(w, "%s %s\n", paint(enabled, styleYellow+";"+styleBold, "warning:"), fmt.Sprintf(format, args...))
}

// confirm asks a yes/no question. The default answer is no.
func (d Deps) confirm(question string) (bool, error) {
	if d.Confirm != nil {
		return d.Confirm(question)
	}
	return false, errNoTerminal
}

// defaultConfirm asks on the controlling terminal. Reading /dev/tty instead of
// stdin keeps the prompt working under "curl ... | sh", where stdin is the
// script. With no controlling terminal there is no one to ask.
func defaultConfirm(stderr io.Writer, color bool) func(string) (bool, error) {
	return func(question string) (bool, error) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return false, errNoTerminal
		}
		defer tty.Close()
		fmt.Fprintf(tty, "%s %s ", paint(color, styleBold, question), paint(color, styleDim, "[y/N]"))
		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(tty)
			return false, nil
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

// addYesFlags registers --yes and its short form -y on one variable.
func addYesFlags(flags *flag.FlagSet) *bool {
	yes := new(bool)
	flags.BoolVar(yes, "yes", false, "approve stopping running services without a prompt")
	flags.BoolVar(yes, "y", false, "short for --yes")
	return yes
}

// componentList names components for a sentence: "hub", "hub and collector".
func componentList(components []systemd.Component) string {
	names := make([]string, 0, len(components))
	for _, component := range components {
		names = append(names, component.String())
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// activeComponents returns the candidates whose unit is currently active, in
// the order given. A unit whose state cannot be read is treated as inactive:
// the caller then never stops or restarts something it could not see.
func activeComponents(ctx context.Context, manager systemd.Manager, candidates []systemd.Component) []systemd.Component {
	var running []systemd.Component
	for _, component := range candidates {
		active, err := manager.IsActive(ctx, component.UnitName())
		if err == nil && active {
			running = append(running, component)
		}
	}
	return running
}

// approveServiceStop explains which running services an operation must stop
// or restart and asks once. assumeYes (--yes/-y) approves without asking. An
// empty running list needs no approval.
//
// effect completes the sentence "The running hub will ...", for example
// "restart so the new port takes effect".
func (d Deps) approveServiceStop(running []systemd.Component, effect string, assumeYes bool) error {
	if len(running) == 0 {
		return nil
	}
	subject := componentList(running)
	verb := "is"
	if len(running) > 1 {
		verb = "are"
	}
	fmt.Fprintf(d.Stderr, "%s The %s %s running and will %s.\n", d.errStyle(styleCyan+";"+styleBold, "::"), d.errStyle(styleBold, subject), verb, effect)
	if assumeYes {
		return nil
	}
	ok, err := d.confirm("Continue?")
	if errors.Is(err, errNoTerminal) {
		return fmt.Errorf("the running %s must stop for this operation and there is no terminal to confirm on; run again with --yes to approve", subject)
	}
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	return nil
}

// resumeComponents starts the given components again after an operation that
// stopped them, in start order (hub first). It reports every failure.
func (d Deps) resumeComponents(ctx context.Context, manager systemd.Manager, stopped []systemd.Component) error {
	var failures []string
	for _, component := range installComponents {
		if !containsComponent(stopped, component) {
			continue
		}
		if err := manager.Start(ctx, component.UnitName()); err != nil {
			failures = append(failures, fmt.Sprintf("start %s: %v", component, err))
			continue
		}
		fmt.Fprintf(d.Stdout, "  %s %s\n", d.out(styleGreen, "resumed:"), component)
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func containsComponent(list []systemd.Component, component systemd.Component) bool {
	for _, candidate := range list {
		if candidate == component {
			return true
		}
	}
	return false
}

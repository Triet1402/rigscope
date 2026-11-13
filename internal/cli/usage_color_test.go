package cli

import (
	"strings"
	"testing"
)

func TestUsageColor(t *testing.T) {
	var plain, colored strings.Builder
	for _, line := range strings.SplitAfter(usageText, "\n") {
		plain.WriteString(styleUsageLine(false, line))
		colored.WriteString(styleUsageLine(true, line))
	}
	if plain.String() != usageText {
		t.Fatal("uncolored help must equal the help text")
	}
	got := colored.String()
	for _, want := range []string{
		"\x1b[1mInstallation:\x1b[0m",
		"  \x1b[36;1minstall\x1b[0m [--role",
		"  \x1b[36;1mcollector enroll\x1b[0m [--config",
		"  \x1b[36;1mstart|stop|restart\x1b[0m [hub|collector]",
		"  \x1b[36;1mversion\x1b[0m    Print the version",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("colored help lacks %q:\n%s", want, got)
		}
	}
	if stripped := strings.NewReplacer("\x1b[1m", "", "\x1b[36;1m", "", "\x1b[0m", "").Replace(got); stripped != usageText {
		t.Fatal("coloring changed the help text")
	}
}

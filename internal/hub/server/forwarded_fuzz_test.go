package server

import (
	"strings"
	"testing"
)

// FuzzParseForwardedElements checks that the strict RFC 7239 parser never
// panics and that every header it accepts is structurally well formed: at least
// one element, each element with at least one parameter, token-shaped lowercase
// names, and no duplicate names. Any input it rejects is discarded wholesale by
// the caller, which is the safe direction.
func FuzzParseForwardedElements(f *testing.F) {
	seeds := []string{
		`for=192.0.2.43`,
		`for=192.0.2.43, for=198.51.100.17`,
		`for="[2001:db8:cafe::17]:4711"`,
		`for=203.0.113.9;proto=https;host="hub.example.test"`,
		`for=203.0.113.9;proto=https;host="hub.example.test", for=10.1.2.3;proto=https`,
		`for=198.51.100.66;ext="`,
		`for=198.51.100.66;ext=", for=10.1.2.3`,
		`for=198.51.100.66;;proto=https`,
		`for=198.51.100.66;proto=https;proto=http`,
		`for="a\"b\\c"`,
		` `,
		`,`,
		`for=`,
		`;`,
		"for=1.2.3.4;ext=\"a\x01b\"",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		elements, err := parseForwardedElements([]string{value})
		if err != nil {
			return
		}
		if len(elements) == 0 {
			t.Fatalf("accepted %q without any element", value)
		}
		for _, element := range elements {
			if len(element) == 0 {
				t.Fatalf("accepted %q with an empty element", value)
			}
			for name := range element {
				if name == "" {
					t.Fatalf("accepted %q with an empty parameter name", value)
				}
				if name != strings.ToLower(name) {
					t.Fatalf("accepted %q with a non-canonical name %q", value, name)
				}
				for index := 0; index < len(name); index++ {
					if !isTokenCharacter(name[index]) {
						t.Fatalf("accepted %q with a non-token name %q", value, name)
					}
				}
			}
		}
	})
}

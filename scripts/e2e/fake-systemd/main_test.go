package main

import (
	"reflect"
	"testing"
)

func TestSplitUnitCommand(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "installer quoting",
			input: `"/home/me/.local/bin/rigscope" hub --config "/home/me/.config/rigscope/config.json"`,
			want:  []string{"/home/me/.local/bin/rigscope", "hub", "--config", "/home/me/.config/rigscope/config.json"},
		},
		{
			name:  "escaped percent and dollar",
			input: `"/bin/sh" "-c" "echo 100%% $$HOME"`,
			want:  []string{"/bin/sh", "-c", "echo 100% $HOME"},
		},
		{
			name:  "spaces inside quotes",
			input: `"/bin/echo" "a b" c`,
			want:  []string{"/bin/echo", "a b", "c"},
		},
		{
			name:  "escaped quote",
			input: `"/bin/echo" "a\"b"`,
			want:  []string{"/bin/echo", `a"b`},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := splitUnitCommand(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("splitUnitCommand(%q) = %#v; want %#v", test.input, got, test.want)
			}
		})
	}
}

func TestSplitUnitCommandRejectsUnterminatedQuote(t *testing.T) {
	if _, err := splitUnitCommand(`"/bin/echo`); err == nil {
		t.Fatal("unterminated quote was accepted")
	}
}

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// docFiles are the user-facing documents that must agree with the CLI. Rewrite
// specifications and retired 0.2 guides are deliberately not parsed: they
// describe other binaries or are kept for historical reference.
var docFiles = []string{
	"../../README.md",
	"../../SECURITY.md",
	"../../docs/installation.md",
	"../../docs/configuration.md",
	"../../docs/security.md",
	"../../docs/monitoring.md",
	"../../docs/telemetry-storage.md",
}

// TestDocumentedCLIExists parses every fenced command block in the 1.0 user
// documentation and checks that each `rigscope <subcommand> --flag` names a
// real subcommand and a flag declared by that subcommand's flag set. The flag
// sets are read from this package's source, so adding or renaming a flag without
// updating the docs fails here.
func TestDocumentedCLIExists(t *testing.T) {
	commands := cliFlagSets(t)
	if len(commands) == 0 {
		t.Fatal("no CLI commands were discovered in internal/cli")
	}
	usages := 0
	for _, path := range docFiles {
		for _, usage := range parseDocUsages(t, path) {
			usages++
			flags, ok := commands[usage.command]
			if !ok {
				t.Errorf("%s:%d: rigscope %s is not a CLI command", path, usage.line, usage.command)
				continue
			}
			if usage.flag == "" {
				continue
			}
			if !flags[usage.flag] && !globalFlags[usage.flag] {
				t.Errorf("%s:%d: rigscope %s does not accept %s", path, usage.line, usage.command, usage.flag)
			}
		}
	}
	if usages == 0 {
		t.Fatal("no rigscope commands were found in the user documentation")
	}
}

var globalFlags = map[string]bool{
	"--help":    true,
	"--version": true,
	"-h":        true,
	"-v":        true,
}

// cliFlagSets maps each subcommand to the flags its flag set declares. The
// shared service verb maps to start, stop and restart.
func cliFlagSets(t *testing.T) map[string]map[string]bool {
	t.Helper()
	commands := map[string]map[string]bool{
		"version": {},
		"license": {},
		"help":    {},
		"start":   {},
		"stop":    {},
		"restart": {},
	}
	functionCommands := map[string][]string{
		"runInstall":               {"install"},
		"runConfigure":             {"configure"},
		"runUninstall":             {"uninstall"},
		"runCleanupLegacyHardware": {"cleanup-legacy-hardware"},
		"runStatus":                {"status"},
		"runService":               {"start", "stop", "restart"},
		"runLogs":                  {"logs"},
		"runUpgrade":               {"upgrade"},
		"runRollback":              {"rollback"},
		"runMigrate":               {"migrate"},
		"runDoctor":                {"doctor"},
		"runHub":                   {"hub"},
		"runResetPassword":         {"reset-password"},
		"runCollector":             {"collector"},
		"runCollectorEnroll":       {"collector enroll"},
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list internal/cli sources: %v", err)
	}
	seen := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			names, ok := functionCommands[function.Name.Name]
			if !ok {
				continue
			}
			seen[function.Name.Name] = true
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				// addYesFlags(flags) declares --yes and its short form -y.
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "addYesFlags" {
					for _, command := range names {
						if commands[command] == nil {
							commands[command] = map[string]bool{}
						}
						commands[command]["--yes"] = true
						commands[command]["-y"] = true
					}
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				receiver, ok := selector.X.(*ast.Ident)
				if !ok || receiver.Name != "flags" {
					return true
				}
				switch selector.Sel.Name {
				case "String", "Bool", "Int", "Func":
				default:
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				literal, ok := call.Args[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				name, err := strconv.Unquote(literal.Value)
				if err != nil || name == "" {
					return true
				}
				for _, command := range names {
					if commands[command] == nil {
						commands[command] = map[string]bool{}
					}
					commands[command]["--"+name] = true
				}
				return true
			})
		}
	}
	for name := range functionCommands {
		if !seen[name] {
			t.Errorf("CLI function %s was not found; update the docs test", name)
		}
	}
	return commands
}

type docUsage struct {
	command string
	flag    string
	line    int
}

// parseDocUsages extracts `rigscope` invocations from fenced blocks. Shell
// continuations are joined first, installer one-liners are treated as
// `rigscope install`, and pipeline stages are inspected independently.
func parseDocUsages(t *testing.T, path string) []docUsage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	inFence := false
	var usages []docUsage
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		start := index + 1
		command := strings.TrimSpace(strings.TrimSuffix(lines[index], "\\"))
		for strings.HasSuffix(strings.TrimSpace(lines[index]), "\\") && index+1 < len(lines) {
			index++
			command += " " + strings.TrimSpace(strings.TrimSuffix(lines[index], "\\"))
		}
		command = strings.TrimSpace(command)
		if command == "" || strings.HasPrefix(command, "#") {
			continue
		}
		command = strings.TrimPrefix(command, "$ ")
		usages = append(usages, parseDocLine(command, start)...)
	}
	return usages
}

func parseDocLine(line string, lineNumber int) []docUsage {
	if comment := strings.Index(line, " #"); comment >= 0 {
		line = line[:comment]
	}
	// A website installer one-liner forwards everything after `--` to
	// `rigscope install`.
	if strings.Contains(line, "install.sh") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field != "--" {
				continue
			}
			return parseFlags(fields[index+1:], "install", lineNumber)
		}
		return nil
	}
	var usages []docUsage
	for _, stage := range splitShellStages(line) {
		fields := strings.Fields(stage)
		for index := 0; index < len(fields); index++ {
			field := fields[index]
			if field != "rigscope" && !strings.HasSuffix(field, "/rigscope") {
				continue
			}
			if index+1 >= len(fields) {
				break
			}
			next := fields[index+1]
			if strings.HasPrefix(next, "-") {
				usages = append(usages, parseFlags(fields[index+1:], "version", lineNumber)...)
				break
			}
			command := next
			consumed := index + 2
			if command == "collector" && consumed < len(fields) && fields[consumed] == "enroll" {
				command = "collector enroll"
				consumed++
			}
			usages = append(usages, docUsage{command: command, line: lineNumber})
			usages = append(usages, parseFlags(fields[consumed:], command, lineNumber)...)
			break
		}
	}
	return usages
}

func parseFlags(fields []string, command string, lineNumber int) []docUsage {
	var usages []docUsage
	afterDoubleDash := false
	for _, field := range fields {
		if field == "|" || field == "&&" || field == "||" || field == ";" ||
			field == ">" || field == ">>" || field == "2>" || field == "#" ||
			field == "rigscope" {
			break
		}
		if field == "--" {
			afterDoubleDash = true
			continue
		}
		if afterDoubleDash || !strings.HasPrefix(field, "--") || field == "--" {
			continue
		}
		name := strings.SplitN(field, "=", 2)[0]
		usages = append(usages, docUsage{command: command, flag: name, line: lineNumber})
	}
	return usages
}

func splitShellStages(line string) []string {
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00")
	return strings.Split(replacer.Replace(line), "\x00")
}

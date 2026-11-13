package cli

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// ownerUsername is the first administrator a fresh hub gets.
const ownerUsername = "owner"

// passwordChoice is a new account password and whether the operator typed it.
// A generated password must be shown; a chosen one never is.
type passwordChoice struct {
	password string
	chosen   bool
}

func generatePassword() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// choosePassword decides a new password for username before anything is
// changed. --password-stdin reads one line from standard input. Otherwise, on
// a terminal and without --yes, it asks twice with echo off; an empty answer
// generates one. With no terminal it generates one.
func (d Deps) choosePassword(username string, fromStdin, assumeYes bool) (passwordChoice, error) {
	if fromStdin {
		reader := d.Stdin
		if reader == nil {
			reader = os.Stdin
		}
		line, err := bufio.NewReader(io.LimitReader(reader, 64<<10)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return passwordChoice{}, fmt.Errorf("read password from stdin: %w", err)
		}
		password := strings.TrimRight(line, "\r\n")
		if password == "" {
			return passwordChoice{}, errors.New("--password-stdin: standard input has no password")
		}
		if _, err := auth.HashPassword(password); err != nil {
			return passwordChoice{}, err
		}
		return passwordChoice{password: password, chosen: true}, nil
	}
	if !assumeYes && d.ReadPassword != nil {
		first, err := d.ReadPassword(fmt.Sprintf("New password for %s (leave empty to generate one): ", username))
		switch {
		case errors.Is(err, errNoTerminal):
		case err != nil:
			return passwordChoice{}, err
		case first != "":
			if _, err := auth.HashPassword(first); err != nil {
				return passwordChoice{}, err
			}
			second, err := d.ReadPassword("Repeat the password: ")
			if err != nil {
				return passwordChoice{}, err
			}
			if second != first {
				return passwordChoice{}, errors.New("the passwords do not match; nothing was changed")
			}
			return passwordChoice{password: first, chosen: true}, nil
		}
	}
	password, err := generatePassword()
	if err != nil {
		return passwordChoice{}, err
	}
	return passwordChoice{password: password}, nil
}

// defaultReadPassword reads one line from the controlling terminal with echo
// turned off. Like the stop prompt it uses /dev/tty, so it works under
// "curl ... | sh".
func defaultReadPassword(color bool) func(string) (string, error) {
	return func(prompt string) (string, error) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return "", errNoTerminal
		}
		defer tty.Close()
		fd := int(tty.Fd())
		saved, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			return "", errNoTerminal
		}
		quiet := *saved
		quiet.Lflag &^= unix.ECHO
		quiet.Lflag |= unix.ICANON | unix.ISIG
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
			return "", fmt.Errorf("turn off terminal echo: %w", err)
		}
		defer func() {
			_ = unix.IoctlSetTermios(fd, unix.TCSETS, saved)
			fmt.Fprintln(tty)
		}()
		fmt.Fprint(tty, paint(color, styleBold, prompt))
		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
}

// ownerBootstrapPath is the hub's one-time browser account import file.
func ownerBootstrapPath(paths config.Paths) string {
	return filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), browserBootstrapFileName)
}

// hubIsFresh reports whether the hub has never started: no database and no
// account import waiting. Only then does install set the owner password.
func hubIsFresh(paths config.Paths) bool {
	dir := paths.ComponentStateDir(systemd.Hub.String())
	return !fileExists(filepath.Join(dir, hubDatabaseFile)) && !fileExists(filepath.Join(dir, browserBootstrapFileName))
}

// writeOwnerBootstrap stores the owner's password hash for the hub's first
// start. The hub imports it once instead of generating and logging a password,
// so the password never reaches the journal.
func writeOwnerBootstrap(deps Deps, paths config.Paths, m *manifest, password string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	var document struct {
		Users []map[string]any `json:"users"`
	}
	document.Users = []map[string]any{{
		"username":     ownerUsername,
		"passwordHash": hash,
		"machineIds":   "*",
		"admin":        true,
	}}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode owner account: %w", err)
	}
	payload = append(payload, '\n')
	if err := os.MkdirAll(paths.ComponentStateDir(systemd.Hub.String()), 0o700); err != nil {
		return fmt.Errorf("create hub state directory: %w", err)
	}
	var prepare func(*os.File) error
	if paths.Scope == config.ScopeSystem {
		prepare = func(file *os.File) error {
			return deps.chownFile(file, systemd.HubUser, systemd.SharedGroup)
		}
	}
	if err := publishManifestedFile(deps, paths, m, ownerBootstrapPath(paths), kindOwnerBootstrap, payload, 0o600, prepare); err != nil {
		return fmt.Errorf("write owner account: %w", err)
	}
	return nil
}

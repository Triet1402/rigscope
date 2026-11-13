package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProductEnvironAllowlist proves that nothing from the invoking shell but
// the executable lookup, locale and timezone reaches a product process.
func TestProductEnvironAllowlist(t *testing.T) {
	ambient := map[string]string{
		"RIGSCOPE_HUB_PORT":              "9999",
		"RIGSCOPE_HUB_DATABASE_PATH":     "/somewhere",
		"RIGSCOPE_HUB_FLUSH_MS":          "1",
		"RIGSCOPE_HUB_DISABLE_BOOTSTRAP": "1",
		"RIGSCOPE_VERSION":               "9.9.9",
		"RIGSCOPE_INSTALL_BASE_URL":      "https://host.invalid",
		"RIGSCOPE_RELEASE_BASE_URL":      "https://host.invalid",
		"RIGSCOPE_UPGRADE_REEXEC":        "1",
		"RIGSCOPE_INSTALL_LOCK_FD":       "3",
		"DBUS_SESSION_BUS_ADDRESS":       "unix:path=/run/user/1000/bus",
		"SYSTEMD_LOG_LEVEL":              "debug",
		"XDG_CONFIG_HOME":                "/host/config",
		"XDG_DATA_HOME":                  "/host/data",
		"XDG_STATE_HOME":                 "/host/state",
		"XDG_RUNTIME_DIR":                "/run/user/1000",
		"SSL_CERT_FILE":                  "/host/ca.pem",
		"SSL_CERT_DIR":                   "/host/certs",
		"CURL_CA_BUNDLE":                 "/host/ca.pem",
		"HTTP_PROXY":                     "http://proxy.invalid:3128",
		"http_proxy":                     "http://proxy.invalid:3128",
		"HTTPS_PROXY":                    "http://proxy.invalid:3128",
		"https_proxy":                    "http://proxy.invalid:3128",
		"ALL_PROXY":                      "socks5://proxy.invalid:1080",
		"all_proxy":                      "socks5://proxy.invalid:1080",
		"NO_PROXY":                       "localhost",
		"no_proxy":                       "localhost",
		"REQUEST_METHOD":                 "GET",
		"CURL_HOME":                      "/host/curl",
		"WGETRC":                         "/host/wgetrc",
		"TAR_OPTIONS":                    "--checkpoint=1",
		"GZIP":                           "-9",
		"ENV":                            "/host/env",
		"BASH_ENV":                       "/host/bashenv",
	}
	for key, value := range ambient {
		t.Setenv(key, value)
	}
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "C.UTF-8")
	t.Setenv("TZ", "UTC")

	environ := productEnviron(map[string]string{"HOME": "/fixture/home"})

	for key := range ambient {
		if value, ok := lookupEnv(environ, key); ok {
			t.Errorf("ambient %s=%q reached the product environment", key, value)
		}
	}
	if value, ok := lookupEnv(environ, "HOME"); !ok || value != "/fixture/home" {
		t.Errorf("HOME = %q, %v; want /fixture/home", value, ok)
	}
	if value, ok := lookupEnv(environ, "LANG"); !ok || value != "C.UTF-8" {
		t.Errorf("LANG = %q, %v; want the allowlisted value", value, ok)
	}
	if count := countEnv(environ, "HOME"); count != 1 {
		t.Errorf("HOME appears %d times, want exactly once", count)
	}
}

// TestTestEnvDropsAmbientRigscope is the regression test from the review: a
// RIGSCOPE_HUB_DATABASE_PATH in the parent process must not reach the installed
// product, while the fixture's explicit settings do.
func TestTestEnvDropsAmbientRigscope(t *testing.T) {
	t.Setenv("RIGSCOPE_HUB_DATABASE_PATH", "/somewhere")
	t.Setenv("RIGSCOPE_HUB_PORT", "9999")
	t.Setenv("RIGSCOPE_RELEASE_BASE_URL", "https://host.invalid/releases")
	t.Setenv("XDG_CONFIG_HOME", "/host/config")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")

	root := t.TempDir()
	env := &testEnv{
		home:       filepath.Join(root, "home"),
		fakeBin:    filepath.Join(root, "fake-bin"),
		fakeRoot:   filepath.Join(root, "fake-systemd"),
		runtimeDir: filepath.Join(root, "run"),
		tmpDir:     filepath.Join(root, "tmp"),
		caFile:     filepath.Join(root, "ca.pem"),
		releaseURL: "https://127.0.0.1:1",
	}
	environ := env.environ()

	for _, key := range []string{
		"RIGSCOPE_HUB_DATABASE_PATH",
		"RIGSCOPE_HUB_PORT",
		"DBUS_SESSION_BUS_ADDRESS",
	} {
		if value, ok := lookupEnv(environ, key); ok {
			t.Errorf("ambient %s=%q reached the fixture environment", key, value)
		}
	}
	if value, _ := lookupEnv(environ, "RIGSCOPE_HUB_DATABASE_PATH"); value == "/somewhere" {
		t.Fatal("RIGSCOPE_HUB_DATABASE_PATH from the parent reached the product process")
	}

	want := map[string]string{
		"HOME":                       env.home,
		"XDG_CONFIG_HOME":            env.configHome(),
		"XDG_RUNTIME_DIR":            env.runtimeDir,
		"TMPDIR":                     env.tmpDir,
		"RIGSCOPE_FAKE_SYSTEMD_ROOT": env.fakeRoot,
		"RIGSCOPE_RELEASE_BASE_URL":  env.releaseURL + "/releases",
		"RIGSCOPE_INSTALL_BASE_URL":  env.releaseURL,
		"RIGSCOPE_VERSION":           versionOne,
		"SSL_CERT_FILE":              env.caFile,
	}
	for key, value := range want {
		if got, ok := lookupEnv(environ, key); !ok || got != value {
			t.Errorf("%s = %q, %v; want %q", key, got, ok, value)
		}
		if count := countEnv(environ, key); count != 1 {
			t.Errorf("%s appears %d times, want exactly once", key, count)
		}
	}
	path, ok := lookupEnv(environ, "PATH")
	first := ""
	if parts := strings.Split(path, string(os.PathListSeparator)); len(parts) > 0 {
		first = parts[0]
	}
	if !ok || first != env.fakeBin {
		t.Errorf("PATH = %q, want the fake bin directory first", path)
	}
}

// TestSanitizedPath keeps absolute entries and drops relative or duplicate
// ones so a tool in the current directory cannot win over the fake systemd.
func TestSanitizedPath(t *testing.T) {
	separator := string(os.PathListSeparator)
	got := sanitizedPath("relative" + separator + "/a" + separator + "" + separator + "/b" + separator + "/a")
	want := "/a" + separator + "/b"
	if got != want {
		t.Fatalf("sanitizedPath = %q, want %q", got, want)
	}
}

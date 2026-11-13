package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProductEnvDropsAmbientOverrides is the release-smoke regression test: a
// RIGSCOPE_HUB_DATABASE_PATH (or any other runtime/test override) exported by
// the verifying shell must not reach the release binary.
func TestProductEnvDropsAmbientOverrides(t *testing.T) {
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
		"XDG_STATE_HOME":                 "/host/state",
		"XDG_RUNTIME_DIR":                "/run/user/1000",
		"SSL_CERT_FILE":                  "/host/ca.pem",
		"SSL_CERT_DIR":                   "/host/certs",
		"CURL_CA_BUNDLE":                 "/host/ca.pem",
		"HTTPS_PROXY":                    "http://proxy.invalid:3128",
		"https_proxy":                    "http://proxy.invalid:3128",
		"NO_PROXY":                       "localhost",
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
	t.Setenv("TZ", "UTC")

	environ := productEnv(map[string]string{"HOME": "/fixture/home"})
	find := func(key string) (string, bool) {
		prefix := key + "="
		for _, entry := range environ {
			if strings.HasPrefix(entry, prefix) {
				return strings.TrimPrefix(entry, prefix), true
			}
		}
		return "", false
	}

	for key := range ambient {
		if value, ok := find(key); ok {
			t.Errorf("ambient %s=%q reached the product environment", key, value)
		}
	}
	if value, ok := find("HOME"); !ok || value != "/fixture/home" {
		t.Errorf("HOME = %q, %v; want /fixture/home", value, ok)
	}
	if value, ok := find("TZ"); !ok || value != "UTC" {
		t.Errorf("TZ = %q, %v; want the allowlisted value", value, ok)
	}
}

func TestSanitizedPath(t *testing.T) {
	separator := string(os.PathListSeparator)
	got := sanitizedPath("relative" + separator + "/a" + separator + "" + separator + "/b" + separator + "/a")
	want := "/a" + separator + "/b"
	if got != want {
		t.Fatalf("sanitizedPath = %q, want %q", got, want)
	}
	if !filepath.IsAbs(strings.Split(got, separator)[0]) {
		t.Fatalf("sanitized path starts with a relative entry: %q", got)
	}
}

// TestCheckHubReapsOnFailureUsedToLeak guards the shape of the fix: a hub
// that cannot serve must not be left running. The command here exits at once,
// so checkHub returns through its early failure path; the deferred bounded stop
// still has to reap the child.
func TestCheckHubReapsExitedChild(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "rigscope")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := checkHub(binary, "1.0.0", "")
	if err == nil {
		t.Fatal("checkHub accepted a binary that never serves the UI")
	}
	if !strings.Contains(err.Error(), "before serving") {
		t.Fatalf("unexpected error: %v", err)
	}
}

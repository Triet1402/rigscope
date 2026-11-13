package auth

import (
	"net/http"
	"strconv"
)

// Cookie names. Remote HTTPS sessions use the `__Host-` prefix so the browser
// enforces a Secure, host-only, Path=/ cookie; a copied loopback cookie cannot
// authenticate against a remote origin.
const (
	SessionCookieName       = "rigscope_session"
	RemoteSessionCookieName = "__Host-rigscope_session"
)

// CookieName returns the cookie name for the request scheme.
func CookieName(secure bool) string {
	if secure {
		return RemoteSessionCookieName
	}
	return SessionCookieName
}

// sessionCookie renders the exact contract attributes. Secure is present only
// for HTTPS requests; Max-Age is the fixed remaining lifetime in seconds.
func sessionCookie(name, value string, maxAgeSeconds int, secure bool) string {
	prefix := ""
	if secure {
		prefix = "Secure; "
	}
	return name + "=" + value + "; " + prefix +
		"HttpOnly; SameSite=Strict; Path=/; Max-Age=" + strconv.Itoa(maxAgeSeconds)
}

// clearedSessionCookie renders the matching deletion cookie.
func clearedSessionCookie(name string, secure bool) string {
	prefix := ""
	if secure {
		prefix = "Secure; "
	}
	return name + "=; " + prefix + "HttpOnly; SameSite=Strict; Path=/; Max-Age=0"
}

func setSessionCookie(w http.ResponseWriter, value string, maxAgeSeconds int, secure bool) {
	w.Header().Add("Set-Cookie", sessionCookie(CookieName(secure), value, maxAgeSeconds, secure))
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	w.Header().Add("Set-Cookie", clearedSessionCookie(CookieName(secure), secure))
}

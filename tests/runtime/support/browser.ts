import assert from "node:assert/strict";

// Deliberately public fixture credentials, used only by isolated loopback hubs.
export const BROWSER_USERNAME = "demo-viewer";
export const BROWSER_PASSWORD = "demo-password-browser-local-only";

export async function loginBrowser(
  baseUrl: string,
  username = BROWSER_USERNAME,
  password = BROWSER_PASSWORD,
): Promise<{ headers: { cookie: string }; csrfToken: string }> {
  const response = await fetch(`${baseUrl}/api/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Origin: baseUrl },
    body: JSON.stringify({ username, password }),
    signal: AbortSignal.timeout(5_000),
  });
  const body = await response.json() as { username?: string; csrfToken?: string };
  assert.equal(response.status, 200, `fixture login failed: ${JSON.stringify(body)}`);
  assert.equal(body.username, username);
  assert.equal(typeof body.csrfToken, "string");
  const setCookie = response.headers.get("set-cookie");
  assert.ok(setCookie, "login must issue a session cookie");
  assert.match(setCookie, /HttpOnly/i);
  assert.match(setCookie, /SameSite=Strict/i);
  assert.match(setCookie, /Path=\//i);
  return { headers: { cookie: setCookie.split(";", 1)[0] }, csrfToken: body.csrfToken! };
}

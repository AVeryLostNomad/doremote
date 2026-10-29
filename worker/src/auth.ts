import type { Env } from "./index";
import { htmlResponse, escapeHtml } from "./connect";

export const SESSION_COOKIE = "dr_session";
export const OAUTH_COOKIE = "dr_oauth";
const SESSION_TTL_SECONDS = 30 * 24 * 60 * 60;
const OAUTH_TTL_SECONDS = 10 * 60;

export interface SessionClaims {
  typ: "session";
  sub: string;
  login: string;
  name: string;
  avatar: string;
  iat: number;
  exp: number;
}

export interface ConnectorClaims {
  typ: "connector";
  sub: string;
  mid: string;
  iat: number;
}

interface OAuthStateClaims {
  typ: "oauth";
  state: string;
  next: string;
  iat: number;
  exp: number;
}

// ---------------------------------------------------------------------------
// HS256 JWT (WebCrypto)
// ---------------------------------------------------------------------------

const encoder = new TextEncoder();
const decoder = new TextDecoder();
const keyCache = new Map<string, Promise<CryptoKey>>();

function hmacKey(secret: string): Promise<CryptoKey> {
  let key = keyCache.get(secret);
  if (!key) {
    key = crypto.subtle.importKey("raw", encoder.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, [
      "sign",
      "verify",
    ]);
    keyCache.set(secret, key);
  }
  return key;
}

function base64UrlEncode(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function base64UrlDecode(input: string): Uint8Array | null {
  if (!/^[A-Za-z0-9_-]*$/.test(input) || input.length % 4 === 1) return null;
  const padded = input.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - (input.length % 4)) % 4);
  try {
    const binary = atob(padded);
    const out = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
    return out;
  } catch {
    return null;
  }
}

const JWT_HEADER = base64UrlEncode(encoder.encode(JSON.stringify({ alg: "HS256", typ: "JWT" })));

export async function signJWT(payload: object, secret: string): Promise<string> {
  requireSecret(secret);
  const body = base64UrlEncode(encoder.encode(JSON.stringify(payload)));
  const signingInput = `${JWT_HEADER}.${body}`;
  const sig = await crypto.subtle.sign("HMAC", await hmacKey(secret), encoder.encode(signingInput));
  return `${signingInput}.${base64UrlEncode(new Uint8Array(sig))}`;
}

export async function verifyJWT(token: string, secret: string): Promise<Record<string, unknown> | null> {
  requireSecret(secret);
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  const [headerPart, bodyPart, sigPart] = parts;
  const headerBytes = base64UrlDecode(headerPart);
  const bodyBytes = base64UrlDecode(bodyPart);
  const sig = base64UrlDecode(sigPart);
  if (!headerBytes || !bodyBytes || !sig) return null;

  let header: unknown;
  let payload: unknown;
  try {
    header = JSON.parse(decoder.decode(headerBytes));
    payload = JSON.parse(decoder.decode(bodyBytes));
  } catch {
    return null;
  }
  if (!isRecord(header) || header.alg !== "HS256") return null;
  if (!isRecord(payload)) return null;

  const valid = await crypto.subtle.verify(
    "HMAC",
    await hmacKey(secret),
    sig,
    encoder.encode(`${headerPart}.${bodyPart}`),
  );
  if (!valid) return null;

  if (payload.exp !== undefined) {
    if (typeof payload.exp !== "number" || payload.exp <= nowSeconds()) return null;
  }
  return payload;
}

function requireSecret(secret: string): void {
  if (!secret || secret.length < 16) throw new Error("SESSION_SECRET is missing or too short");
}

export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function nowSeconds(): number {
  return Math.floor(Date.now() / 1000);
}

// ---------------------------------------------------------------------------
// Cookies
// ---------------------------------------------------------------------------

export function parseCookies(header: string | null): Map<string, string> {
  const out = new Map<string, string>();
  if (!header) return out;
  for (const part of header.split(";")) {
    const eq = part.indexOf("=");
    if (eq < 0) continue;
    const name = part.slice(0, eq).trim();
    if (!name || out.has(name)) continue;
    let value = part.slice(eq + 1).trim();
    if (value.startsWith('"') && value.endsWith('"') && value.length >= 2) value = value.slice(1, -1);
    try {
      out.set(name, decodeURIComponent(value));
    } catch {
      out.set(name, value);
    }
  }
  return out;
}

/** Secure is omitted only for plain-http local development (wrangler dev). */
export function isInsecureLocal(url: URL): boolean {
  return url.protocol === "http:" && (url.hostname === "localhost" || url.hostname === "127.0.0.1");
}

interface CookieOptions {
  maxAge: number;
  path: string;
}

export function serializeCookie(name: string, value: string, url: URL, opts: CookieOptions): string {
  let cookie = `${name}=${encodeURIComponent(value)}; Path=${opts.path}; Max-Age=${opts.maxAge}; HttpOnly; SameSite=Lax`;
  if (opts.maxAge <= 0) cookie += "; Expires=Thu, 01 Jan 1970 00:00:00 GMT";
  if (!isInsecureLocal(url)) cookie += "; Secure";
  return cookie;
}

export function clearSessionCookie(url: URL): string {
  return serializeCookie(SESSION_COOKIE, "", url, { maxAge: 0, path: "/" });
}

// ---------------------------------------------------------------------------
// Session / connector token helpers
// ---------------------------------------------------------------------------

export async function readSession(request: Request, env: Env): Promise<SessionClaims | null> {
  const token = parseCookies(request.headers.get("Cookie")).get(SESSION_COOKIE);
  if (!token) return null;
  const claims = await verifyJWT(token, env.SESSION_SECRET);
  if (
    !claims ||
    claims.typ !== "session" ||
    typeof claims.sub !== "string" ||
    !claims.sub ||
    typeof claims.exp !== "number"
  ) {
    return null;
  }
  return {
    typ: "session",
    sub: claims.sub,
    login: typeof claims.login === "string" ? claims.login : "",
    name: typeof claims.name === "string" ? claims.name : "",
    avatar: typeof claims.avatar === "string" ? claims.avatar : "",
    iat: typeof claims.iat === "number" ? claims.iat : 0,
    exp: claims.exp,
  };
}

export async function readConnectorToken(request: Request, env: Env): Promise<ConnectorClaims | null> {
  const auth = request.headers.get("Authorization");
  const match = auth ? /^Bearer\s+(\S+)$/i.exec(auth) : null;
  if (!match) return null;
  const claims = await verifyJWT(match[1], env.SESSION_SECRET);
  if (
    !claims ||
    claims.typ !== "connector" ||
    typeof claims.sub !== "string" ||
    !claims.sub ||
    typeof claims.mid !== "string" ||
    !claims.mid
  ) {
    return null;
  }
  return {
    typ: "connector",
    sub: claims.sub,
    mid: claims.mid,
    iat: typeof claims.iat === "number" ? claims.iat : 0,
  };
}

export function mintConnectorToken(sub: string, mid: string, env: Env): Promise<string> {
  const claims: ConnectorClaims = { typ: "connector", sub, mid, iat: nowSeconds() };
  return signJWT(claims, env.SESSION_SECRET);
}

/** Returns `next` if it is a safe same-origin path, otherwise "/". */
export function safeNext(next: string | null | undefined): string {
  if (!next || next.length > 2048) return "/";
  if (!next.startsWith("/") || next.startsWith("//")) return "/";
  // Browsers treat "\" as "/" in URLs ("/\evil.com" → "//evil.com"); also reject control characters.
  if (/[\\\u0000-\u001f\u007f]/.test(next)) return "/";
  return next;
}

// ---------------------------------------------------------------------------
// GitHub OAuth
// ---------------------------------------------------------------------------

function randomState(): string {
  const bytes = new Uint8Array(24);
  crypto.getRandomValues(bytes);
  return base64UrlEncode(bytes);
}

function noStore(headers: Headers): Headers {
  headers.set("Cache-Control", "no-store");
  return headers;
}

export async function handleLogin(url: URL, env: Env): Promise<Response> {
  if (!env.GITHUB_CLIENT_ID) throw new Error("GITHUB_CLIENT_ID is not configured");
  const next = safeNext(url.searchParams.get("next"));
  const state = randomState();
  const iat = nowSeconds();
  const stateClaims: OAuthStateClaims = { typ: "oauth", state, next, iat, exp: iat + OAUTH_TTL_SECONDS };
  const cookieValue = await signJWT(stateClaims, env.SESSION_SECRET);

  const authorize = new URL("https://github.com/login/oauth/authorize");
  // Secrets pasted into `wrangler secret put` often carry stray whitespace; OAuth IDs never do.
  authorize.searchParams.set("client_id", env.GITHUB_CLIENT_ID.trim());
  authorize.searchParams.set("redirect_uri", `${url.origin}/auth/callback`);
  authorize.searchParams.set("scope", "read:user");
  authorize.searchParams.set("state", state);
  authorize.searchParams.set("allow_signup", "true");

  const headers = noStore(new Headers({ Location: authorize.toString() }));
  headers.append(
    "Set-Cookie",
    serializeCookie(OAUTH_COOKIE, cookieValue, url, { maxAge: OAUTH_TTL_SECONDS, path: "/auth" }),
  );
  return new Response(null, { status: 302, headers });
}

function authError(url: URL, message: string, status = 400): Response {
  const res = htmlResponse(
    "Sign-in failed",
    `<h1>Sign-in failed</h1><p>${escapeHtml(message)}</p><p><a class="button" href="/auth/login">Try again</a> <a href="/">Home</a></p>`,
    status,
  );
  res.headers.append("Set-Cookie", serializeCookie(OAUTH_COOKIE, "", url, { maxAge: 0, path: "/auth" }));
  return res;
}

interface GitHubUser {
  id: number;
  login: string;
  name: string | null;
  avatar_url: string;
}

export async function handleCallback(request: Request, url: URL, env: Env): Promise<Response> {
  const ghError = url.searchParams.get("error");
  if (ghError) {
    return authError(url, url.searchParams.get("error_description") || `GitHub returned "${ghError}".`);
  }
  const code = url.searchParams.get("code");
  const state = url.searchParams.get("state");
  if (!code || !state) return authError(url, "Missing code or state.");

  const cookie = parseCookies(request.headers.get("Cookie")).get(OAUTH_COOKIE);
  const stateClaims = cookie ? await verifyJWT(cookie, env.SESSION_SECRET) : null;
  if (
    !stateClaims ||
    stateClaims.typ !== "oauth" ||
    typeof stateClaims.state !== "string" ||
    stateClaims.state !== state
  ) {
    return authError(url, "Sign-in session expired or did not match. Please try again.");
  }
  const next = safeNext(typeof stateClaims.next === "string" ? stateClaims.next : "/");

  const tokenRes = await fetch("https://github.com/login/oauth/access_token", {
    method: "POST",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/x-www-form-urlencoded",
      "User-Agent": "doremote",
    },
    body: new URLSearchParams({
      client_id: env.GITHUB_CLIENT_ID.trim(),
      client_secret: env.GITHUB_CLIENT_SECRET.trim(),
      code,
      redirect_uri: `${url.origin}/auth/callback`,
    }),
  });
  if (!tokenRes.ok) return authError(url, `GitHub token exchange failed (HTTP ${tokenRes.status}).`, 502);
  const tokenBody = (await tokenRes.json()) as { access_token?: string; error?: string; error_description?: string };
  if (!tokenBody.access_token) {
    return authError(url, tokenBody.error_description || tokenBody.error || "GitHub did not return an access token.");
  }

  const userRes = await fetch("https://api.github.com/user", {
    headers: {
      Authorization: `Bearer ${tokenBody.access_token}`,
      Accept: "application/vnd.github+json",
      "User-Agent": "doremote",
      "X-GitHub-Api-Version": "2022-11-28",
    },
  });
  if (!userRes.ok) return authError(url, `Could not load your GitHub profile (HTTP ${userRes.status}).`, 502);
  const user = (await userRes.json()) as GitHubUser;
  if (typeof user.id !== "number" || typeof user.login !== "string") {
    return authError(url, "GitHub returned an unexpected profile.", 502);
  }

  const iat = nowSeconds();
  const session: SessionClaims = {
    typ: "session",
    sub: String(user.id),
    login: user.login,
    name: user.name || user.login,
    avatar: typeof user.avatar_url === "string" ? user.avatar_url : "",
    iat,
    exp: iat + SESSION_TTL_SECONDS,
  };
  const sessionToken = await signJWT(session, env.SESSION_SECRET);

  const headers = noStore(new Headers({ Location: next }));
  headers.append(
    "Set-Cookie",
    serializeCookie(SESSION_COOKIE, sessionToken, url, { maxAge: SESSION_TTL_SECONDS, path: "/" }),
  );
  headers.append("Set-Cookie", serializeCookie(OAUTH_COOKIE, "", url, { maxAge: 0, path: "/auth" }));
  return new Response(null, { status: 302, headers });
}

export function handleLogout(url: URL): Response {
  const headers = noStore(new Headers());
  headers.append("Set-Cookie", clearSessionCookie(url));
  return new Response(null, { status: 204, headers });
}

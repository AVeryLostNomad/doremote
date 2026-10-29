import type { Env } from "./index";
import { mintConnectorToken, readSession, type SessionClaims } from "./auth";
import { hubFor, normalizeMachineName, type MachineOS } from "./hub";

const PAGE_CSP =
  "default-src 'none'; style-src 'unsafe-inline'; img-src https://avatars.githubusercontent.com; base-uri 'none'; frame-ancestors 'none'";

export function escapeHtml(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

/** Minimal standalone HTML page. `bodyHtml` must already be escaped. */
export function htmlResponse(title: string, bodyHtml: string, status = 200): Response {
  const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>${escapeHtml(title)} · doremote</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 24px;
    font: 15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    background: radial-gradient(ellipse at top, #182033 0%, #0b0e14 60%); color: #e6e9ef; }
  .card { width: 100%; max-width: 440px; background: #131823; border: 1px solid #262e3f; border-radius: 16px;
    padding: 28px; box-shadow: 0 20px 60px rgba(0,0,0,.45); }
  .brand { font-weight: 700; letter-spacing: .02em; color: #7aa2ff; margin-bottom: 16px; }
  h1 { font-size: 20px; margin: 0 0 16px; }
  p { margin: 0 0 14px; color: #c3c9d6; }
  .muted { color: #8a93a6; font-size: 13px; }
  .machine { display: flex; gap: 12px; align-items: center; padding: 14px; border-radius: 12px;
    background: #0e121b; border: 1px solid #222a3a; margin-bottom: 16px; }
  .machine .os { font-size: 12px; padding: 2px 8px; border-radius: 999px; background: #1f2a44; color: #9fb7ff; }
  .machine .name { font-weight: 600; word-break: break-all; }
  .user { display: inline-flex; align-items: center; gap: 6px; vertical-align: middle; }
  .user img { width: 20px; height: 20px; border-radius: 50%; }
  .actions { display: flex; gap: 12px; align-items: center; margin-top: 20px; }
  button, .button { appearance: none; border: 0; border-radius: 10px; padding: 10px 18px; font: inherit; font-weight: 600;
    background: #3b6cf6; color: #fff; cursor: pointer; text-decoration: none; display: inline-block; }
  button:hover, .button:hover { background: #4d7bff; }
  a { color: #9fb7ff; }
</style>
</head>
<body><main class="card">${bodyHtml}</main></body>
</html>`;
  return new Response(html, {
    status,
    headers: {
      "Content-Type": "text/html; charset=utf-8",
      "Content-Security-Policy": PAGE_CSP,
      "X-Frame-Options": "DENY",
      "X-Content-Type-Options": "nosniff",
      "Referrer-Policy": "no-referrer",
      "Cache-Control": "no-store",
    },
  });
}

interface ConnectParams {
  port: number;
  state: string;
  name: string;
  os: MachineOS;
}

const OS_LABELS: Record<MachineOS, string> = { darwin: "macOS", windows: "Windows" };

function parseConnectParams(get: (key: string) => string | null): ConnectParams | string {
  const portRaw = get("port") ?? "";
  if (!/^\d{1,5}$/.test(portRaw)) return "Invalid port.";
  const port = Number(portRaw);
  if (port < 1024 || port > 65535) return "Invalid port.";

  const state = get("state") ?? "";
  if (!/^[A-Za-z0-9_-]{16,128}$/.test(state)) return "Invalid state.";

  const os = get("os");
  if (os !== "darwin" && os !== "windows") return "Unsupported operating system.";

  const name = normalizeMachineName(get("name") ?? "") || "Unnamed machine";
  return { port, state, name, os };
}

function errorPage(message: string, status: number): Response {
  return htmlResponse(
    "Cannot connect",
    `<div class="brand">doremote</div><h1>Cannot connect this computer</h1><p>${escapeHtml(message)}</p>` +
      `<p class="muted">Restart the doremote connector to try pairing again.</p><div class="actions"><a class="button" href="/">Go to doremote</a></div>`,
    status,
  );
}

function approvePage(params: ConnectParams, session: SessionClaims): Response {
  const avatar = session.avatar.startsWith("https://avatars.githubusercontent.com/")
    ? `<img src="${escapeHtml(session.avatar)}" alt="">`
    : "";
  const body = `<div class="brand">doremote</div>
<h1>Connect this computer?</h1>
<div class="machine"><span class="os">${escapeHtml(OS_LABELS[params.os])}</span><span class="name">${escapeHtml(params.name)}</span></div>
<p>Signed in as <span class="user">${avatar}<b>${escapeHtml(session.login)}</b></span>.
Approving lets the doremote connector on <b>${escapeHtml(params.name)}</b> share its screen and accept mouse and keyboard input from your account.</p>
<p class="muted">Only continue if you just started the connector yourself. It will receive its access token on 127.0.0.1:${escapeHtml(String(params.port))}.</p>
<form method="post" action="/connect">
<input type="hidden" name="port" value="${escapeHtml(String(params.port))}">
<input type="hidden" name="state" value="${escapeHtml(params.state)}">
<input type="hidden" name="name" value="${escapeHtml(params.name)}">
<input type="hidden" name="os" value="${escapeHtml(params.os)}">
<div class="actions"><button type="submit">Connect</button><a href="/">Cancel</a></div>
</form>`;
  return htmlResponse("Connect this computer", body);
}

export async function handleConnect(request: Request, url: URL, env: Env): Promise<Response> {
  if (request.method === "GET" || request.method === "HEAD") {
    const params = parseConnectParams((k) => url.searchParams.get(k));
    if (typeof params === "string") return errorPage(params, 400);
    const session = await readSession(request, env);
    if (!session) {
      const login = `/auth/login?next=${encodeURIComponent(url.pathname + url.search)}`;
      return new Response(null, { status: 302, headers: { Location: login, "Cache-Control": "no-store" } });
    }
    return approvePage(params, session);
  }

  if (request.method !== "POST") {
    return new Response("Method Not Allowed", { status: 405, headers: { Allow: "GET, POST" } });
  }

  if (request.headers.get("Origin") !== url.origin) return errorPage("Request origin not allowed.", 403);
  const session = await readSession(request, env);
  if (!session) return errorPage("You are not signed in. Restart the connector to pair again.", 401);

  let form: FormData;
  try {
    form = await request.formData();
  } catch {
    return errorPage("Malformed request.", 400);
  }
  const params = parseConnectParams((k) => {
    const v = form.get(k);
    return typeof v === "string" ? v : null;
  });
  if (typeof params === "string") return errorPage(params, 400);

  const machine = await hubFor(env.HUB, session.sub).createMachine(params.name, params.os);
  const token = await mintConnectorToken(session.sub, machine.id, env);

  const callback = new URL(`http://127.0.0.1:${params.port}/callback`);
  callback.searchParams.set("state", params.state);
  callback.searchParams.set("token", token);
  callback.searchParams.set("mid", machine.id);
  return new Response(null, {
    status: 302,
    headers: { Location: callback.toString(), "Cache-Control": "no-store", "Referrer-Policy": "no-referrer" },
  });
}

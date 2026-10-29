import {
  handleCallback,
  handleLogin,
  handleLogout,
  isRecord,
  readConnectorToken,
  readSession,
  type SessionClaims,
} from "./auth";
import { handleConnect } from "./connect";
import {
  Hub,
  HUB_MID_HEADER,
  HUB_ROLE_HEADER,
  HUB_SID_HEADER,
  MAX_NAME_LENGTH,
  hubFor,
  normalizeMachineName,
  type MachinePatch,
  type SocketRole,
} from "./hub";

export { Hub };

export interface Env {
  HUB: DurableObjectNamespace<Hub>;
  ASSETS: Fetcher;
  SESSION_SECRET: string;
  GITHUB_CLIENT_ID: string;
  GITHUB_CLIENT_SECRET: string;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const MACHINE_PATH_RE = /^\/api\/machines\/([^/]+)$/;
const VIEW_PATH_RE = /^\/api\/machines\/([^/]+)\/view$/;

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" },
  });
}

function jsonError(status: number, message: string): Response {
  return json({ error: message }, status);
}

function isWebSocketUpgrade(request: Request): boolean {
  return request.headers.get("Upgrade")?.toLowerCase() === "websocket";
}

/** Completes the upgrade and immediately closes with `code`, so browser clients can observe the reason. */
function rejectWebSocket(code: number, reason: string): Response {
  const { 0: client, 1: server } = new WebSocketPair();
  server.accept();
  server.close(code, reason);
  return new Response(null, { status: 101, webSocket: client });
}

/** Rejects cross-origin state-changing requests. Requests without Origin (non-browser) pass. */
function crossOrigin(request: Request, url: URL): boolean {
  const origin = request.headers.get("Origin");
  return origin !== null && origin !== url.origin;
}

function forwardToHub(env: Env, userId: string, role: SocketRole, mid: string, sid?: string): Promise<Response> {
  const headers = new Headers({ Upgrade: "websocket", [HUB_ROLE_HEADER]: role, [HUB_MID_HEADER]: mid });
  if (sid) headers.set(HUB_SID_HEADER, sid);
  return hubFor(env.HUB, userId).fetch("https://hub.internal/ws", { headers });
}

function validateDimension(value: unknown, min: number, max: number, label: string): number | string {
  if (typeof value !== "number" || !Number.isInteger(value)) return `${label} must be an integer`;
  if (value < min || value > max) return `${label} must be between ${min} and ${max}`;
  if (value % 2 !== 0) return `${label} must be even`;
  return value;
}

function parsePatch(body: unknown): MachinePatch | string {
  if (!isRecord(body)) return "body must be a JSON object";
  const patch: MachinePatch = {};
  if (body.name !== undefined) {
    if (typeof body.name !== "string") return "name must be a string";
    const name = normalizeMachineName(body.name);
    if (!name) return "name must not be empty";
    if (Array.from(body.name.trim()).length > MAX_NAME_LENGTH) return `name must be at most ${MAX_NAME_LENGTH} characters`;
    patch.name = name;
  }
  if (body.width !== undefined) {
    const width = validateDimension(body.width, 640, 7680, "width");
    if (typeof width === "string") return width;
    patch.width = width;
  }
  if (body.height !== undefined) {
    const height = validateDimension(body.height, 480, 4320, "height");
    if (typeof height === "string") return height;
    patch.height = height;
  }
  return patch;
}

async function handleApi(request: Request, url: URL, env: Env): Promise<Response> {
  const path = url.pathname;
  const method = request.method;

  // Connector sockets authenticate with a bearer connector token.
  if (path === "/api/connector/ws" || path === "/api/connector/stream") {
    if (!isWebSocketUpgrade(request)) return jsonError(426, "expected websocket upgrade");
    const claims = await readConnectorToken(request, env);
    if (!claims || !UUID_RE.test(claims.mid)) return rejectWebSocket(4401, "unauthorized");
    if (path === "/api/connector/ws") return forwardToHub(env, claims.sub, "connector", claims.mid);
    const sid = url.searchParams.get("sid") ?? "";
    if (!UUID_RE.test(sid)) return rejectWebSocket(4404, "session not found");
    return forwardToHub(env, claims.sub, "stream", claims.mid, sid);
  }

  const viewMatch = VIEW_PATH_RE.exec(path);
  if (viewMatch) {
    if (method !== "GET" || !isWebSocketUpgrade(request)) return jsonError(426, "expected websocket upgrade");
    if (request.headers.get("Origin") !== url.origin) return jsonError(403, "origin not allowed");
    const session = await readSession(request, env);
    if (!session) return rejectWebSocket(4401, "unauthorized");
    const mid = viewMatch[1];
    if (!UUID_RE.test(mid)) return rejectWebSocket(4404, "machine not found");
    return forwardToHub(env, session.sub, "viewer", mid);
  }

  if (method !== "GET" && method !== "HEAD" && crossOrigin(request, url)) return jsonError(403, "origin not allowed");

  const session = await readSession(request, env);

  if (path === "/api/me") {
    if (method !== "GET") return jsonError(405, "method not allowed");
    if (!session) return jsonError(401, "unauthorized");
    return json(publicUser(session));
  }

  if (path === "/api/machines") {
    if (method !== "GET") return jsonError(405, "method not allowed");
    if (!session) return jsonError(401, "unauthorized");
    return json(await hubFor(env.HUB, session.sub).listMachines());
  }

  const machineMatch = MACHINE_PATH_RE.exec(path);
  if (machineMatch) {
    if (!session) return jsonError(401, "unauthorized");
    const mid = machineMatch[1];
    if (!UUID_RE.test(mid)) return jsonError(404, "machine not found");
    const hub = hubFor(env.HUB, session.sub);

    if (method === "PATCH") {
      let body: unknown;
      try {
        body = await request.json();
      } catch {
        return jsonError(400, "invalid JSON");
      }
      const patch = parsePatch(body);
      if (typeof patch === "string") return jsonError(400, patch);
      const machine = await hub.updateMachine(mid, patch);
      return machine ? json(machine) : jsonError(404, "machine not found");
    }
    if (method === "DELETE") {
      const existed = await hub.deleteMachine(mid);
      return existed ? new Response(null, { status: 204 }) : jsonError(404, "machine not found");
    }
    return jsonError(405, "method not allowed");
  }

  return jsonError(404, "not found");
}

function publicUser(session: SessionClaims): { id: string; login: string; name: string; avatar: string } {
  return { id: session.sub, login: session.login, name: session.name, avatar: session.avatar };
}

async function handleAuth(request: Request, url: URL, env: Env): Promise<Response> {
  switch (url.pathname) {
    case "/auth/login":
      if (request.method !== "GET") return new Response("Method Not Allowed", { status: 405 });
      return handleLogin(url, env);
    case "/auth/callback":
      if (request.method !== "GET") return new Response("Method Not Allowed", { status: 405 });
      return handleCallback(request, url, env);
    case "/auth/logout":
      if (request.method !== "POST") return new Response("Method Not Allowed", { status: 405 });
      if (crossOrigin(request, url)) return new Response("Forbidden", { status: 403 });
      return handleLogout(url);
    default:
      return new Response("Not Found", { status: 404 });
  }
}

export default {
  async fetch(request, env): Promise<Response> {
    const url = new URL(request.url);
    try {
      if (url.pathname.startsWith("/api/")) return await handleApi(request, url, env);
      if (url.pathname.startsWith("/auth/")) return await handleAuth(request, url, env);
      if (url.pathname === "/connect") return await handleConnect(request, url, env);
      return await env.ASSETS.fetch(request);
    } catch (err) {
      console.error("request failed", url.pathname, err);
      if (isWebSocketUpgrade(request)) return rejectWebSocket(1011, "internal error");
      return jsonError(500, "internal error");
    }
  },
} satisfies ExportedHandler<Env>;

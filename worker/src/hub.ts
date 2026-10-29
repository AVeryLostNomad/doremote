import { DurableObject } from "cloudflare:workers";
import type { Env } from "./index";

export type MachineOS = "darwin" | "windows";

export interface Machine {
  id: string;
  name: string;
  os: string;
  width: number;
  height: number;
  createdAt: number;
  lastSeen: number | null;
  version: string;
}

export interface MachineView extends Machine {
  online: boolean;
}

export interface MachinePatch {
  name?: string;
  width?: number;
  height?: number;
}

export type SocketRole = "connector" | "viewer" | "stream";

/** Internal headers the worker sets when forwarding a WebSocket upgrade to the hub. */
export const HUB_ROLE_HEADER = "X-Doremote-Role";
export const HUB_MID_HEADER = "X-Doremote-Mid";
export const HUB_SID_HEADER = "X-Doremote-Sid";

export const DEFAULT_WIDTH = 1920;
export const DEFAULT_HEIGHT = 1080;
export const MAX_NAME_LENGTH = 64;

interface Attachment {
  role: SocketRole;
  mid: string;
  sid?: string;
}

const OPEN = 1;
const MACHINE_PREFIX = "machine:";
const encoder = new TextEncoder();

export function hubFor(ns: DurableObjectNamespace<Hub>, userId: string): DurableObjectStub<Hub> {
  return ns.get(ns.idFromName(`user:${userId}`));
}

/** Strips control characters, collapses whitespace, trims and truncates to MAX_NAME_LENGTH code points. */
export function normalizeMachineName(raw: string): string {
  const cleaned = raw.replace(/[\u0000-\u001f\u007f-\u009f]/g, " ").replace(/\s+/g, " ").trim();
  return Array.from(cleaned).slice(0, MAX_NAME_LENGTH).join("").trim();
}

function attachment(ws: WebSocket): Attachment | null {
  try {
    return (ws.deserializeAttachment() as Attachment | null) ?? null;
  } catch {
    return null;
  }
}

/** Close codes a server may send; reserved/invalid codes (1005, 1006, 1015, …) become 1000. */
function sendableCloseCode(code: number): number {
  if (code === 1000 || code === 1001 || code === 1011) return code;
  if (code >= 3000 && code <= 4999) return code;
  return 1000;
}

function truncateReason(reason: string): string {
  // Close reasons are limited to 123 UTF-8 bytes.
  let out = reason;
  while (encoder.encode(out).length > 123) out = Array.from(out).slice(0, -1).join("");
  return out;
}

function closeSocket(ws: WebSocket, code: number, reason: string): void {
  try {
    ws.close(sendableCloseCode(code), truncateReason(reason));
  } catch {
    // Already closed or closing.
  }
}

function sendTo(ws: WebSocket, message: string | ArrayBuffer): void {
  try {
    ws.send(message);
  } catch {
    // Socket went away between lookup and send; its close handler cleans up.
  }
}

function rejectSocket(server: WebSocket, code: number, reason: string): void {
  server.accept();
  closeSocket(server, code, reason);
}

/**
 * One Hub per GitHub user. Stores the user's machines and relays sessions between
 * connector control/stream sockets and browser viewer sockets using hibernatable WebSockets.
 */
export class Hub extends DurableObject<Env> {
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair('{"t":"ping"}', '{"t":"pong"}'));
  }

  // -------------------------------------------------------------------------
  // RPC API (called by the worker after authenticating the user)
  // -------------------------------------------------------------------------

  async listMachines(): Promise<MachineView[]> {
    const stored = await this.ctx.storage.list<Machine>({ prefix: MACHINE_PREFIX });
    return Array.from(stored.values(), (m) => this.view(m)).sort((a, b) => a.createdAt - b.createdAt);
  }

  async createMachine(name: string, os: MachineOS): Promise<MachineView> {
    const machine: Machine = {
      id: crypto.randomUUID(),
      name: normalizeMachineName(name),
      os,
      width: DEFAULT_WIDTH,
      height: DEFAULT_HEIGHT,
      createdAt: Date.now(),
      lastSeen: null,
      version: "",
    };
    await this.ctx.storage.put(MACHINE_PREFIX + machine.id, machine);
    return this.view(machine);
  }

  /** `patch` must already be validated by the caller. Returns null if the machine does not exist. */
  async updateMachine(mid: string, patch: MachinePatch): Promise<MachineView | null> {
    const machine = await this.getMachine(mid);
    if (!machine) return null;
    if (patch.name !== undefined) machine.name = normalizeMachineName(patch.name);
    if (patch.width !== undefined) machine.width = patch.width;
    if (patch.height !== undefined) machine.height = patch.height;
    await this.ctx.storage.put(MACHINE_PREFIX + mid, machine);
    return this.view(machine);
  }

  /** Deletes the machine (revoking its connector token) and closes all of its sockets. */
  async deleteMachine(mid: string): Promise<boolean> {
    const existed = await this.ctx.storage.delete(MACHINE_PREFIX + mid);
    for (const ws of this.sockets(`m:${mid}`, "connector")) closeSocket(ws, 4401, "machine removed");
    this.endMachineSessions(mid, 4404, "machine removed");
    return existed;
  }

  // -------------------------------------------------------------------------
  // WebSocket upgrades (forwarded by the worker with internal role headers)
  // -------------------------------------------------------------------------

  async fetch(request: Request): Promise<Response> {
    if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
      return new Response("Expected WebSocket upgrade", { status: 426 });
    }
    const role = request.headers.get(HUB_ROLE_HEADER);
    const mid = request.headers.get(HUB_MID_HEADER) ?? "";
    const { 0: client, 1: server } = new WebSocketPair();

    switch (role) {
      case "connector":
        await this.acceptConnector(server, mid);
        break;
      case "viewer":
        await this.acceptViewer(server, mid);
        break;
      case "stream":
        this.acceptStream(server, mid, request.headers.get(HUB_SID_HEADER) ?? "");
        break;
      default:
        return new Response("Unknown role", { status: 400 });
    }
    return new Response(null, { status: 101, webSocket: client });
  }

  private async acceptConnector(server: WebSocket, mid: string): Promise<void> {
    const machine = await this.getMachine(mid);
    if (!machine) {
      rejectSocket(server, 4401, "unknown machine");
      return;
    }
    const previous = this.sockets(`m:${mid}`, "connector");
    if (previous.length > 0) {
      for (const ws of previous) closeSocket(ws, 4409, "replaced");
      // Sessions belonged to the replaced control connection.
      this.endMachineSessions(mid, 4404, "machine reconnected");
    }

    this.ctx.acceptWebSocket(server, ["connector", `m:${mid}`]);
    server.serializeAttachment({ role: "connector", mid } satisfies Attachment);

    machine.lastSeen = Date.now();
    await this.ctx.storage.put(MACHINE_PREFIX + mid, machine);
  }

  private async acceptViewer(server: WebSocket, mid: string): Promise<void> {
    const machine = await this.getMachine(mid);
    if (!machine) {
      rejectSocket(server, 4404, "machine not found");
      return;
    }
    const control = this.sockets(`m:${mid}`, "connector")[0];
    if (!control) {
      rejectSocket(server, 4404, "machine offline");
      return;
    }

    for (const old of this.sockets(`m:${mid}`, "viewer")) {
      const oldSid = attachment(old)?.sid;
      closeSocket(old, 4409, "replaced");
      if (oldSid) this.endSession(oldSid, mid, { streamCode: 1000, streamReason: "replaced" });
    }

    const sid = crypto.randomUUID();
    this.ctx.acceptWebSocket(server, ["viewer", `s:${sid}`, `m:${mid}`]);
    server.serializeAttachment({ role: "viewer", mid, sid } satisfies Attachment);

    sendTo(control, JSON.stringify({ t: "session", sid, width: machine.width, height: machine.height }));
    sendTo(server, JSON.stringify({ t: "status", message: "starting" }));
  }

  private acceptStream(server: WebSocket, mid: string, sid: string): void {
    const viewer = sid ? this.sockets(`s:${sid}`, "viewer").find((v) => attachment(v)?.mid === mid) : undefined;
    if (!viewer) {
      rejectSocket(server, 4404, "session not found");
      return;
    }
    for (const old of this.sockets(`s:${sid}`, "stream")) closeSocket(old, 4409, "replaced");

    this.ctx.acceptWebSocket(server, ["stream", `s:${sid}`]);
    server.serializeAttachment({ role: "stream", mid, sid } satisfies Attachment);
  }

  // -------------------------------------------------------------------------
  // Hibernation handlers
  // -------------------------------------------------------------------------

  async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const att = attachment(ws);
    if (!att) return;
    switch (att.role) {
      case "viewer": {
        const stream = this.sockets(`s:${att.sid}`, "stream")[0];
        if (stream) sendTo(stream, message);
        return;
      }
      case "stream": {
        const viewer = this.sockets(`s:${att.sid}`, "viewer")[0];
        if (viewer) sendTo(viewer, message);
        return;
      }
      case "connector":
        if (typeof message === "string") await this.handleControlMessage(ws, att.mid, message);
        return;
    }
  }

  async webSocketClose(ws: WebSocket, code: number, reason: string): Promise<void> {
    closeSocket(ws, code, reason);
    await this.socketGone(ws, code, reason);
  }

  async webSocketError(ws: WebSocket): Promise<void> {
    closeSocket(ws, 1011, "error");
    await this.socketGone(ws, 1011, "");
  }

  private async handleControlMessage(ws: WebSocket, mid: string, raw: string): Promise<void> {
    let msg: { t?: unknown; name?: unknown; os?: unknown; version?: unknown };
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    if (typeof msg !== "object" || msg === null) return;

    if (msg.t === "ping") {
      sendTo(ws, '{"t":"pong"}');
      return;
    }
    if (msg.t !== "hello") return;

    const machine = await this.getMachine(mid);
    if (!machine) {
      closeSocket(ws, 4401, "unknown machine");
      return;
    }
    if (msg.os === "darwin" || msg.os === "windows") machine.os = msg.os;
    if (typeof msg.version === "string") machine.version = msg.version.slice(0, 64);
    if (!machine.name && typeof msg.name === "string") machine.name = normalizeMachineName(msg.name);
    machine.lastSeen = Date.now();
    await this.ctx.storage.put(MACHINE_PREFIX + mid, machine);
  }

  private async socketGone(ws: WebSocket, code: number, reason: string): Promise<void> {
    const att = attachment(ws);
    if (!att) return;
    switch (att.role) {
      case "connector": {
        // A replaced connector must not tear down sessions owned by its successor.
        const successor = this.sockets(`m:${att.mid}`, "connector").some((other) => other !== ws);
        if (!successor) this.endMachineSessions(att.mid, 4404, "machine went offline");
        const machine = await this.getMachine(att.mid);
        if (machine) {
          machine.lastSeen = Date.now();
          await this.ctx.storage.put(MACHINE_PREFIX + att.mid, machine);
        }
        return;
      }
      case "viewer":
        if (att.sid) this.endSession(att.sid, att.mid, { streamCode: 1000, streamReason: "viewer disconnected" });
        return;
      case "stream":
        if (att.sid) {
          const forwarded = code >= 4000 && code <= 4999;
          this.endSession(att.sid, att.mid, {
            viewerCode: forwarded ? code : 1000,
            viewerReason: reason || "session ended",
          });
        }
        return;
    }
  }

  // -------------------------------------------------------------------------
  // Helpers
  // -------------------------------------------------------------------------

  /** Open sockets carrying `tag` whose attachment role is `role`. */
  private sockets(tag: string, role: SocketRole): WebSocket[] {
    return this.ctx.getWebSockets(tag).filter((ws) => ws.readyState === OPEN && attachment(ws)?.role === role);
  }

  /**
   * Closes the remaining viewer/stream sockets of a session and tells the connector it ended.
   * Sockets whose close codes are omitted are left alone (they are the ones already closing).
   */
  private endSession(
    sid: string,
    mid: string,
    opts: { viewerCode?: number; viewerReason?: string; streamCode?: number; streamReason?: string },
  ): void {
    if (opts.viewerCode !== undefined) {
      for (const v of this.sockets(`s:${sid}`, "viewer")) closeSocket(v, opts.viewerCode, opts.viewerReason ?? "");
    }
    if (opts.streamCode !== undefined) {
      for (const s of this.sockets(`s:${sid}`, "stream")) closeSocket(s, opts.streamCode, opts.streamReason ?? "");
    }
    const control = this.sockets(`m:${mid}`, "connector")[0];
    if (control) sendTo(control, JSON.stringify({ t: "end", sid }));
  }

  /** Closes every viewer (with `code`/`reason`) and every stream of a machine. */
  private endMachineSessions(mid: string, code: number, reason: string): void {
    for (const viewer of this.sockets(`m:${mid}`, "viewer")) closeSocket(viewer, code, reason);
    for (const stream of this.sockets("stream", "stream")) {
      if (attachment(stream)?.mid === mid) closeSocket(stream, 1001, reason);
    }
  }

  private async getMachine(mid: string): Promise<Machine | null> {
    if (!mid) return null;
    return (await this.ctx.storage.get<Machine>(MACHINE_PREFIX + mid)) ?? null;
  }

  private view(machine: Machine): MachineView {
    return { ...machine, online: this.sockets(`m:${machine.id}`, "connector").length > 0 };
  }
}

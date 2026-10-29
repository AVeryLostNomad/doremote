// doremote portal — vanilla ES module, no build step.

const app = document.getElementById("app");

const PRESETS = [
  [1280, 720],
  [1600, 900],
  [1920, 1080],
  [2560, 1440],
  [3840, 2160],
];
const LIMITS = { minW: 640, maxW: 7680, minH: 480, maxH: 4320 };
const OS_LABELS = { darwin: "macOS", windows: "Windows" };
const REFRESH_MS = 5000;

let me = null;
let cleanupView = null;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

class AuthError extends Error {}

async function api(path, { method = "GET", body } = {}) {
  const init = { method, credentials: "same-origin", headers: {} };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  if (res.status === 401) throw new AuthError("Not signed in");
  if (!res.ok) {
    let message = "";
    try {
      message = (await res.json()).error ?? "";
    } catch {
      // Non-JSON error body.
    }
    throw new Error(message || `Request failed (HTTP ${res.status})`);
  }
  return res.status === 204 ? null : res.json();
}

/** Tiny DOM builder: h("div", { class: "x", onclick: fn, hidden: true }, "text", child). */
function h(tag, props = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith("on") && typeof value === "function") el.addEventListener(key.slice(2), value);
    else if (key === "class") el.className = value;
    else if (key in el && typeof value !== "string") el[key] = value;
    else el.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return el;
}

function timeAgo(ms) {
  if (!ms) return "never";
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 45) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const hrs = Math.round(m / 60);
  if (hrs < 24) return `${hrs} h ago`;
  const days = Math.round(hrs / 24);
  if (days < 30) return `${days} d ago`;
  return new Date(ms).toLocaleDateString();
}

function capitalize(text) {
  return text ? text[0].toUpperCase() + text.slice(1) : text;
}

function signInHref() {
  return `/auth/login?next=${encodeURIComponent(location.pathname + location.search + location.hash)}`;
}

function showSignedOut() {
  me = null;
  route();
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

async function route() {
  if (cleanupView) {
    const cleanup = cleanupView;
    cleanupView = null;
    cleanup();
  }
  if (!me) {
    try {
      me = await api("/api/me");
    } catch (err) {
      if (err instanceof AuthError) return renderSignedOut();
      return renderFatal(err);
    }
  }
  const match = /^#\/view\/([0-9a-f-]{36})$/.exec(location.hash);
  if (match) renderViewer(match[1]);
  else renderMachines();
}

window.addEventListener("hashchange", route);
route();

// ---------------------------------------------------------------------------
// Signed out / fatal
// ---------------------------------------------------------------------------

function githubIcon() {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("width", "18");
  svg.setAttribute("height", "18");
  svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS(ns, "path");
  path.setAttribute("fill", "currentColor");
  path.setAttribute(
    "d",
    "M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z",
  );
  svg.append(path);
  return svg;
}

function renderSignedOut() {
  app.replaceChildren(
    h(
      "main",
      { class: "hero" },
      h("div", { class: "hero-card" }, [
        h("div", { class: "logo" }, h("span", { class: "logo-mark" }), "doremote"),
        h("h1", {}, "Your computers, from any browser."),
        h(
          "p",
          { class: "muted" },
          "Run the doremote connector on a Mac or Windows PC, pair it with your GitHub account, and control it from here.",
        ),
        h("a", { class: "btn btn-github", href: signInHref() }, githubIcon(), "Sign in with GitHub"),
      ]),
    ),
  );
}

function renderFatal(err) {
  app.replaceChildren(
    h(
      "main",
      { class: "hero" },
      h("div", { class: "hero-card" }, [
        h("div", { class: "logo" }, h("span", { class: "logo-mark" }), "doremote"),
        h("h1", {}, "Something went wrong"),
        h("p", { class: "muted" }, err instanceof Error ? err.message : String(err)),
        h("button", { class: "btn btn-primary", onclick: () => route() }, "Retry"),
      ]),
    ),
  );
}

// ---------------------------------------------------------------------------
// Machine list
// ---------------------------------------------------------------------------

function topbar() {
  const signOut = async () => {
    try {
      await fetch("/auth/logout", { method: "POST", credentials: "same-origin" });
    } finally {
      if (location.hash) history.replaceState(null, "", location.pathname + location.search);
      showSignedOut();
    }
  };
  return h(
    "header",
    { class: "topbar" },
    h("a", { class: "logo", href: "#/" }, h("span", { class: "logo-mark" }), "doremote"),
    h(
      "div",
      { class: "user" },
      me.avatar ? h("img", { class: "avatar", src: me.avatar, alt: "", referrerpolicy: "no-referrer" }) : null,
      h("span", { class: "user-login" }, me.login),
      h("button", { class: "btn btn-ghost btn-sm", onclick: signOut }, "Sign out"),
    ),
  );
}

function renderMachines() {
  const grid = h("div", { class: "machine-grid" });
  const empty = h(
    "div",
    { class: "empty", hidden: true },
    h("div", { class: "empty-icon" }),
    h("h2", {}, "No machines yet"),
    h(
      "p",
      { class: "muted" },
      "Install and start the doremote connector on a Mac or Windows PC. It opens this site to pair with your account; the machine then shows up here.",
    ),
  );
  const banner = h("div", { class: "banner banner-error", hidden: true });
  const loading = h("div", { class: "loading" }, h("div", { class: "spinner" }));

  app.replaceChildren(
    topbar(),
    h(
      "main",
      { class: "container" },
      h("div", { class: "section-head" }, h("h1", {}, "Machines")),
      banner,
      loading,
      grid,
      empty,
    ),
  );

  const cards = new Map();
  let disposed = false;
  let inflight = false;

  const showError = (message) => {
    banner.textContent = message;
    banner.hidden = !message;
  };

  async function refresh() {
    if (inflight || disposed) return;
    inflight = true;
    try {
      const machines = await api("/api/machines");
      if (disposed) return;
      showError("");
      loading.remove();
      const seen = new Set();
      for (const [index, machine] of machines.entries()) {
        seen.add(machine.id);
        let card = cards.get(machine.id);
        if (!card) {
          card = machineCard(machine, { onRemoved: () => removeCard(machine.id), onError: showError });
          cards.set(machine.id, card);
        }
        card.update(machine);
        // Keep server order without detaching nodes that are already in place (preserves input focus).
        const at = grid.children[index] ?? null;
        if (at !== card.el) grid.insertBefore(card.el, at);
      }
      for (const id of [...cards.keys()]) if (!seen.has(id)) removeCard(id);
      empty.hidden = cards.size > 0;
    } catch (err) {
      if (err instanceof AuthError) return showSignedOut();
      if (!disposed) showError(`Couldn't load machines: ${err.message}`);
    } finally {
      inflight = false;
    }
  }

  function removeCard(id) {
    const card = cards.get(id);
    if (!card) return;
    card.el.remove();
    cards.delete(id);
    empty.hidden = cards.size > 0;
  }

  const onVisible = () => {
    if (document.visibilityState === "visible") refresh();
  };
  document.addEventListener("visibilitychange", onVisible);
  const timer = setInterval(() => {
    if (document.visibilityState === "visible") refresh();
  }, REFRESH_MS);
  refresh();

  cleanupView = () => {
    disposed = true;
    clearInterval(timer);
    document.removeEventListener("visibilitychange", onVisible);
  };
}

function presetValue(w, h) {
  return `${w}x${h}`;
}

function validateResolution(w, hgt) {
  if (!Number.isInteger(w) || !Number.isInteger(hgt)) return "Width and height must be whole numbers.";
  if (w < LIMITS.minW || w > LIMITS.maxW) return `Width must be between ${LIMITS.minW} and ${LIMITS.maxW}.`;
  if (hgt < LIMITS.minH || hgt > LIMITS.maxH) return `Height must be between ${LIMITS.minH} and ${LIMITS.maxH}.`;
  if (w % 2 || hgt % 2) return "Width and height must be even.";
  return "";
}

function machineCard(initial, { onRemoved, onError }) {
  let machine = initial;
  let renaming = false;
  let editingCustom = false;
  let busy = false;

  const dot = h("span", { class: "dot" });
  const nameEl = h("div", { class: "machine-name" });
  const nameInput = h("input", {
    class: "input name-input",
    type: "text",
    maxlength: "64",
    hidden: true,
    "aria-label": "Machine name",
  });
  const meta = h("div", { class: "machine-meta" });
  const connectBtn = h("a", { class: "btn btn-primary" }, "Connect");

  const select = h(
    "select",
    { class: "input", "aria-label": "Resolution" },
    PRESETS.map(([w, hgt]) => h("option", { value: presetValue(w, hgt) }, `${w} × ${hgt}`)),
    h("option", { value: "custom" }, "Custom…"),
  );
  const customW = h("input", {
    class: "input num",
    type: "number",
    min: String(LIMITS.minW),
    max: String(LIMITS.maxW),
    step: "2",
    "aria-label": "Width",
  });
  const customH = h("input", {
    class: "input num",
    type: "number",
    min: String(LIMITS.minH),
    max: String(LIMITS.maxH),
    step: "2",
    "aria-label": "Height",
  });
  const applyBtn = h("button", { class: "btn btn-sm" }, "Apply");
  const cancelCustomBtn = h("button", { class: "btn btn-ghost btn-sm" }, "Cancel");
  const custom = h("div", { class: "custom-res", hidden: true }, customW, h("span", { class: "muted" }, "×"), customH, applyBtn, cancelCustomBtn);

  const renameBtn = h("button", { class: "btn btn-ghost btn-sm" }, "Rename");
  const saveNameBtn = h("button", { class: "btn btn-sm", hidden: true }, "Save");
  const cancelNameBtn = h("button", { class: "btn btn-ghost btn-sm", hidden: true }, "Cancel");
  const removeBtn = h("button", { class: "btn btn-ghost btn-sm btn-danger" }, "Remove");

  const el = h(
    "article",
    { class: "machine" },
    h(
      "div",
      { class: "machine-head" },
      h("div", { class: "machine-icon" }, dot),
      h("div", { class: "machine-title" }, nameEl, nameInput, meta),
      connectBtn,
    ),
    h(
      "div",
      { class: "machine-res" },
      h("label", { class: "label" }, "Resolution"),
      h("div", { class: "res-controls" }, select, custom),
    ),
    h("div", { class: "machine-actions" }, renameBtn, saveNameBtn, cancelNameBtn, h("span", { class: "spacer" }), removeBtn),
  );

  function isPreset(w, hgt) {
    return PRESETS.some(([pw, ph]) => pw === w && ph === hgt);
  }

  function syncResolution() {
    if (editingCustom) return;
    const preset = isPreset(machine.width, machine.height);
    if (preset) {
      select.value = presetValue(machine.width, machine.height);
      custom.hidden = true;
    } else {
      select.value = "custom";
      custom.hidden = false;
      customW.value = String(machine.width);
      customH.value = String(machine.height);
    }
  }

  function setBusy(value) {
    busy = value;
    el.classList.toggle("busy", value);
    for (const control of [select, customW, customH, applyBtn, renameBtn, saveNameBtn, removeBtn, nameInput]) {
      control.disabled = value;
    }
  }

  async function patch(body) {
    setBusy(true);
    try {
      const updated = await api(`/api/machines/${machine.id}`, { method: "PATCH", body });
      onError("");
      return updated;
    } catch (err) {
      if (err instanceof AuthError) showSignedOut();
      else onError(`Couldn't update "${machine.name}": ${err.message}`);
      return null;
    } finally {
      setBusy(false);
    }
  }

  async function applyResolution(w, hgt) {
    const problem = validateResolution(w, hgt);
    if (problem) {
      onError(problem);
      return;
    }
    const updated = await patch({ width: w, height: hgt });
    editingCustom = false;
    update(updated ?? machine);
  }

  select.addEventListener("change", () => {
    if (select.value === "custom") {
      editingCustom = true;
      custom.hidden = false;
      customW.value = String(machine.width);
      customH.value = String(machine.height);
      customW.focus();
      return;
    }
    const [w, hgt] = select.value.split("x").map(Number);
    editingCustom = false;
    custom.hidden = true;
    applyResolution(w, hgt);
  });
  for (const input of [customW, customH]) {
    input.addEventListener("focus", () => (editingCustom = true));
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") applyBtn.click();
      if (e.key === "Escape") cancelCustomBtn.click();
    });
  }
  applyBtn.addEventListener("click", () => applyResolution(Number(customW.value), Number(customH.value)));
  cancelCustomBtn.addEventListener("click", () => {
    editingCustom = false;
    syncResolution();
  });

  function setRenaming(value) {
    renaming = value;
    nameEl.hidden = value;
    nameInput.hidden = !value;
    renameBtn.hidden = value;
    saveNameBtn.hidden = !value;
    cancelNameBtn.hidden = !value;
    if (value) {
      nameInput.value = machine.name;
      nameInput.focus();
      nameInput.select();
    }
  }

  async function saveName() {
    const name = nameInput.value.trim();
    if (!name) {
      onError("Name must not be empty.");
      return;
    }
    if (name === machine.name) {
      setRenaming(false);
      return;
    }
    const updated = await patch({ name });
    if (updated) {
      setRenaming(false);
      update(updated);
    }
  }

  renameBtn.addEventListener("click", () => setRenaming(true));
  cancelNameBtn.addEventListener("click", () => setRenaming(false));
  saveNameBtn.addEventListener("click", saveName);
  nameInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") saveName();
    if (e.key === "Escape") setRenaming(false);
  });

  removeBtn.addEventListener("click", async () => {
    const ok = confirm(
      `Remove "${machine.name}"?\n\nIts connector will be signed out and has to be paired again to reconnect.`,
    );
    if (!ok) return;
    setBusy(true);
    try {
      await api(`/api/machines/${machine.id}`, { method: "DELETE" });
      onError("");
      onRemoved();
    } catch (err) {
      if (err instanceof AuthError) showSignedOut();
      else onError(`Couldn't remove "${machine.name}": ${err.message}`);
      setBusy(false);
    }
  });

  function update(next) {
    machine = next;
    const label = machine.name || "Unnamed machine";
    nameEl.textContent = label;
    nameEl.title = label;
    dot.className = `dot ${machine.online ? "dot-online" : "dot-offline"}`;
    dot.title = machine.online ? "Online" : "Offline";
    el.classList.toggle("offline", !machine.online);

    const parts = [OS_LABELS[machine.os] ?? machine.os ?? "Unknown OS"];
    if (machine.version) parts.push(`v${machine.version.replace(/^v/, "")}`);
    parts.push(machine.online ? "Online" : `Last seen ${timeAgo(machine.lastSeen)}`);
    meta.textContent = parts.join(" · ");

    if (machine.online) {
      connectBtn.href = `#/view/${machine.id}`;
      connectBtn.classList.remove("disabled");
      connectBtn.removeAttribute("aria-disabled");
      connectBtn.title = `Connect at ${machine.width} × ${machine.height}`;
    } else {
      connectBtn.removeAttribute("href");
      connectBtn.classList.add("disabled");
      connectBtn.setAttribute("aria-disabled", "true");
      connectBtn.title = "Machine is offline";
    }
    if (!busy) syncResolution();
  }

  return { el, update };
}

// ---------------------------------------------------------------------------
// Viewer
// ---------------------------------------------------------------------------

function closeMessage(code, reason) {
  switch (code) {
    case 4401:
      return "Not authorized. Sign in again or re-pair the machine.";
    case 4404:
      return reason ? `${capitalize(reason)}.` : "The machine is offline.";
    case 4409:
      return "This session was opened in another window.";
    case 1000:
      return reason ? `${capitalize(reason)}.` : "Session ended.";
    case 1006:
      return "Connection lost.";
    default:
      return reason ? `${capitalize(reason)} (code ${code}).` : `Disconnected (code ${code}).`;
  }
}

/**
 * WebCodecs codec string ("avc1.PPCCLL") from the SPS in an Annex B access unit, or "" if the
 * access unit carries no SPS.
 */
function h264Codec(au) {
  const hex = (b) => b.toString(16).padStart(2, "0");
  for (let i = 0; i + 6 < au.length; i++) {
    // NAL units follow a 00 00 01 start code (a 4-byte 00 00 00 01 ends with the same 3 bytes).
    if (au[i] !== 0 || au[i + 1] !== 0 || au[i + 2] !== 1) continue;
    if ((au[i + 3] & 0x1f) === 7) return `avc1.${hex(au[i + 4])}${hex(au[i + 5])}${hex(au[i + 6])}`;
    i += 2;
  }
  return "";
}

function renderViewer(machineId) {
  const controller = new AbortController();
  const { signal } = controller;

  const statusDot = h("span", { class: "dot" });
  const statusText = h("span", { class: "status-text" }, "Connecting…");
  const fpsEl = h("span", { class: "stat" }, "— fps");
  const resEl = h("span", { class: "stat" });
  const titleEl = h("span", { class: "viewer-title" }, "Remote machine");
  const fullscreenBtn = h("button", { class: "btn btn-ghost btn-sm" }, "Fullscreen");
  const disconnectBtn = h("button", { class: "btn btn-sm btn-danger-solid" }, "Disconnect");
  const backBtn = h("a", { class: "btn btn-ghost btn-sm", href: "#/" }, "← Machines");

  const canvas = h("canvas", { class: "screen", tabindex: "0", width: 0, height: 0 });
  const ctx = canvas.getContext("2d", { alpha: false });

  const overlayTitle = h("div", { class: "overlay-title" }, "Connecting…");
  const overlayDetail = h("div", { class: "overlay-detail muted" });
  const reconnectBtn = h("button", { class: "btn btn-primary", hidden: true }, "Reconnect");
  const overlayBack = h("a", { class: "btn btn-ghost", href: "#/", hidden: true }, "Back to machines");
  const overlaySpinner = h("div", { class: "spinner" });
  const overlay = h(
    "div",
    { class: "overlay" },
    h("div", { class: "overlay-card" }, overlaySpinner, overlayTitle, overlayDetail, h("div", { class: "overlay-actions" }, reconnectBtn, overlayBack)),
  );

  const stage = h("div", { class: "stage" }, canvas, overlay);
  const root = h(
    "div",
    { class: "viewer" },
    h(
      "div",
      { class: "toolbar" },
      backBtn,
      titleEl,
      h("div", { class: "status" }, statusDot, statusText),
      h("span", { class: "spacer" }),
      resEl,
      fpsEl,
      fullscreenBtn,
      disconnectBtn,
    ),
    stage,
  );
  app.replaceChildren(root);

  // Machine name for the toolbar (best effort).
  api("/api/machines")
    .then((machines) => {
      const m = machines.find((x) => x.id === machineId);
      if (m) {
        titleEl.textContent = m.name || "Unnamed machine";
        document.title = `${m.name} · doremote`;
      }
    })
    .catch((err) => {
      if (err instanceof AuthError) showSignedOut();
    });

  let ws = null;
  let live = false;
  let lastError = "";
  let frames = 0;
  let bytes = 0;
  const video = createVideoSink();

  function setStatus(kind, text) {
    statusDot.className = `dot dot-${kind}`;
    statusText.textContent = text;
  }

  function showOverlay(title, detail, { spinner = false, actions = false } = {}) {
    overlay.hidden = false;
    overlayTitle.textContent = title;
    overlayDetail.textContent = detail ?? "";
    overlaySpinner.hidden = !spinner;
    reconnectBtn.hidden = !actions;
    overlayBack.hidden = !actions;
  }

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
  }

  // ---- connection --------------------------------------------------------

  function connect() {
    live = false;
    lastError = "";
    video.reset();
    pressed.clear();
    lastMove = null;
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${proto}//${location.host}/api/machines/${encodeURIComponent(machineId)}/view`);
    socket.binaryType = "arraybuffer";
    ws = socket;
    setStatus("pending", "Connecting…");
    showOverlay("Connecting…", "", { spinner: true });

    socket.addEventListener("open", () => {
      if (ws !== socket) return;
      setStatus("pending", "Waiting for machine…");
      showOverlay("Waiting for machine…", "", { spinner: true });
    });
    socket.addEventListener("message", (ev) => {
      if (ws !== socket) return;
      if (typeof ev.data === "string") handleText(ev.data);
      else handleFrame(socket, ev.data);
    });
    socket.addEventListener("close", (ev) => {
      if (ws !== socket) return;
      ws = null;
      live = false;
      video.reset();
      const message = lastError || closeMessage(ev.code, ev.reason);
      setStatus(lastError || ev.code !== 1000 ? "error" : "offline", lastError ? "Error" : "Disconnected");
      showOverlay(lastError ? "Session failed" : "Disconnected", message, { actions: true });
      if (ev.code === 4401) {
        // Session cookie may have expired: re-check on next action.
        me = null;
      }
    });
  }

  function handleText(data) {
    let msg;
    try {
      msg = JSON.parse(data);
    } catch {
      return;
    }
    switch (msg.t) {
      case "status": {
        const text = capitalize(String(msg.message ?? ""));
        setStatus("pending", text === "Starting" ? "Starting session…" : text);
        if (!live) showOverlay(text === "Starting" ? "Starting session…" : text, "", { spinner: true });
        break;
      }
      case "ready": {
        if (msg.codec !== "h264") {
          // Connectors before the H.264 stream send JPEG tiles this viewer can't draw.
          lastError = "The doremote app on this computer is out of date. It checks for updates hourly; choose Check for updates in its menu to update now.";
          setStatus("error", "Error");
          showOverlay("Update needed", lastError, { actions: true });
          ws?.close(1000, "connector out of date");
          break;
        }
        const w = Number(msg.width);
        const hgt = Number(msg.height);
        if (w > 0 && hgt > 0) {
          if (canvas.width !== w || canvas.height !== hgt) {
            canvas.width = w;
            canvas.height = hgt;
          }
          resEl.textContent = `${w} × ${hgt}`;
        }
        setStatus("pending", "Waiting for first frame…");
        if (!live) showOverlay("Waiting for first frame…", "", { spinner: true });
        break;
      }
      case "error":
        lastError = String(msg.message ?? "Unknown error");
        setStatus("error", "Error");
        showOverlay("Session failed", lastError, { actions: true });
        break;
      case "pong":
        break;
    }
  }

  /**
   * Decodes the H.264 stream with WebCodecs and draws each picture as soon as it is decoded.
   * Frame message (little-endian): u8 kind (1 = H.264), u8 flags (bit 0 keyframe), u16 width,
   * u16 height, then one Annex B access unit. Every frame is acknowledged exactly once, in
   * order, when it was drawn or dropped: the connector only sends a few frames ahead of acks
   * and paces its bitrate by how quickly they come back.
   */
  function createVideoSink() {
    let decoder = null;
    let codec = "";
    let width = 0;
    let height = 0;
    let needKey = true;
    let pending = 0; // chunks handed to the decoder whose picture hasn't come out yet
    let timestamp = 0;
    let stallTimer = 0;

    function ack(n = 1) {
      for (let i = 0; i < n; i++) if (ws && ws.readyState === WebSocket.OPEN) ws.send('{"t":"ack"}');
    }

    function drop() {
      clearTimeout(stallTimer);
      if (decoder && decoder.state !== "closed") decoder.close();
      decoder = null;
      codec = "";
      ack(pending);
      pending = 0;
    }

    /** Throws the decoder away and asks the connector for a fresh keyframe. */
    function recover(reason) {
      console.warn("video decoder reset:", reason);
      drop();
      needKey = true;
      send({ t: "keyframe" });
    }

    function draw(frame) {
      pending = Math.max(0, pending - 1);
      clearTimeout(stallTimer);
      if (pending > 0) stallTimer = setTimeout(() => recover("decoder stalled"), 2000);
      try {
        if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
          canvas.width = frame.displayWidth;
          canvas.height = frame.displayHeight;
          resEl.textContent = `${frame.displayWidth} × ${frame.displayHeight}`;
        }
        ctx.drawImage(frame, 0, 0);
      } finally {
        frame.close();
      }
      frames++;
      ack();
      if (!live && ws) {
        live = true;
        overlay.hidden = true;
        setStatus("live", "Live");
        canvas.focus({ preventScroll: true });
      }
    }

    function decoderError(err) {
      if (err.name === "NotSupportedError") unsupported(`${codec}: ${err.message}`);
      else recover(err.message);
    }

    /** The browser can't decode this stream at all: retrying would only loop. */
    function unsupported(detail) {
      drop();
      lastError = `This browser can't decode the stream (H.264 ${detail}).`;
      setStatus("error", "Error");
      showOverlay("Session failed", lastError, { actions: true });
      ws?.close(1000, "unsupported codec");
    }

    function push(buf) {
      const view = new DataView(buf);
      if (buf.byteLength < 6 || view.getUint8(0) !== 1) {
        ack();
        return;
      }
      const key = (view.getUint8(1) & 1) !== 0;
      const w = view.getUint16(2, true);
      const hgt = view.getUint16(4, true);
      const data = new Uint8Array(buf, 6);
      if (needKey && !key) {
        ack(); // undecodable without the keyframe that was lost; one is on its way
        return;
      }
      if (key) {
        const next = h264Codec(data);
        if (next && (!decoder || next !== codec || w !== width || hgt !== height)) {
          try {
            decoder ??= new VideoDecoder({ output: draw, error: decoderError });
            decoder.configure({ codec: next, codedWidth: w, codedHeight: hgt, optimizeForLatency: true });
          } catch (err) {
            ack();
            unsupported(`${next}: ${err.message}`);
            return;
          }
          codec = next;
          width = w;
          height = hgt;
        }
        if (!decoder) {
          ack();
          recover("keyframe without SPS");
          return;
        }
        needKey = false;
      }
      if (!decoder) {
        ack();
        return;
      }
      try {
        decoder.decode(new EncodedVideoChunk({ type: key ? "key" : "delta", timestamp, data }));
      } catch (err) {
        ack();
        recover(err.message);
        return;
      }
      timestamp += 16_667;
      pending++;
      if (pending === 1) {
        clearTimeout(stallTimer);
        stallTimer = setTimeout(() => recover("decoder stalled"), 2000);
      }
    }

    return {
      push,
      /** Forgets all state, without acks: the socket the frames came from is gone or new. */
      reset() {
        pending = 0;
        drop();
        needKey = true;
        timestamp = 0;
      },
    };
  }

  function handleFrame(socket, buf) {
    bytes += buf.byteLength;
    if (ws !== socket) return;
    if (typeof VideoDecoder === "undefined") {
      if (!lastError) {
        lastError = "This browser can't decode the stream: it needs WebCodecs (Chrome/Edge 94+, Safari 16.4+, Firefox 130+).";
        setStatus("error", "Error");
        showOverlay("Unsupported browser", lastError, { actions: true });
      }
      return;
    }
    video.push(buf);
  }

  const statsTimer = setInterval(() => {
    if (live) {
      const mbps = (bytes * 8) / 1e6;
      fpsEl.textContent = `${frames} fps · ${mbps.toFixed(1)} Mbps`;
    }
    frames = 0;
    bytes = 0;
  }, 1000);

  // ---- input mapping -----------------------------------------------------

  /** Maps client coordinates to display pixels, accounting for object-fit: contain letterboxing. */
  function toDisplay(clientX, clientY) {
    const w = canvas.width;
    const hgt = canvas.height;
    const rect = canvas.getBoundingClientRect();
    if (!w || !hgt || !rect.width || !rect.height) return null;
    const scale = Math.min(rect.width / w, rect.height / hgt);
    const left = rect.left + (rect.width - w * scale) / 2;
    const top = rect.top + (rect.height - hgt * scale) / 2;
    const rx = Math.floor((clientX - left) / scale);
    const ry = Math.floor((clientY - top) / scale);
    return {
      x: Math.min(w - 1, Math.max(0, rx)),
      y: Math.min(hgt - 1, Math.max(0, ry)),
      inside: rx >= 0 && ry >= 0 && rx < w && ry < hgt,
    };
  }

  let pendingMove = null;
  let lastMove = null;
  let wheelDx = 0;
  let wheelDy = 0;
  let rafId = 0;
  const pressed = new Set();

  function schedule() {
    if (!rafId) rafId = requestAnimationFrame(flush);
  }

  function flushMove() {
    if (!pendingMove) return;
    const p = pendingMove;
    pendingMove = null;
    if (lastMove && lastMove.x === p.x && lastMove.y === p.y) return;
    lastMove = p;
    send({ t: "move", x: p.x, y: p.y });
  }

  function flush() {
    rafId = 0;
    if (!live) {
      pendingMove = null;
      wheelDx = wheelDy = 0;
      return;
    }
    flushMove();
    if (wheelDx || wheelDy) {
      send({ t: "wheel", dx: Math.round(wheelDx * 100) / 100, dy: Math.round(wheelDy * 100) / 100 });
      wheelDx = wheelDy = 0;
    }
  }

  canvas.addEventListener(
    "pointermove",
    (e) => {
      const p = toDisplay(e.clientX, e.clientY);
      if (!p || (!p.inside && pressed.size === 0)) return;
      pendingMove = { x: p.x, y: p.y };
      schedule();
    },
    { signal },
  );

  canvas.addEventListener(
    "pointerdown",
    (e) => {
      e.preventDefault();
      canvas.focus({ preventScroll: true });
      if (!live || e.button < 0 || e.button > 2) return;
      const p = toDisplay(e.clientX, e.clientY);
      if (!p || !p.inside) return;
      try {
        canvas.setPointerCapture(e.pointerId);
      } catch {
        // Pointer already released.
      }
      pendingMove = { x: p.x, y: p.y };
      flushMove();
      pressed.add(e.button);
      send({ t: "button", b: e.button, down: true, x: p.x, y: p.y });
    },
    { signal },
  );

  const pointerUp = (e) => {
    if (!pressed.has(e.button)) return;
    e.preventDefault();
    pressed.delete(e.button);
    const p = toDisplay(e.clientX, e.clientY) ?? lastMove ?? { x: 0, y: 0 };
    pendingMove = { x: p.x, y: p.y };
    flushMove();
    send({ t: "button", b: e.button, down: false, x: p.x, y: p.y });
  };
  canvas.addEventListener("pointerup", pointerUp, { signal });
  canvas.addEventListener(
    "pointercancel",
    () => {
      for (const b of pressed) {
        const p = lastMove ?? { x: 0, y: 0 };
        send({ t: "button", b, down: false, x: p.x, y: p.y });
      }
      pressed.clear();
    },
    { signal },
  );

  canvas.addEventListener(
    "wheel",
    (e) => {
      e.preventDefault();
      if (!live) return;
      const p = toDisplay(e.clientX, e.clientY);
      if (p && p.inside) {
        pendingMove = { x: p.x, y: p.y };
      }
      const unit = e.deltaMode === WheelEvent.DOM_DELTA_LINE ? 16 : e.deltaMode === WheelEvent.DOM_DELTA_PAGE ? canvas.clientHeight || 800 : 1;
      wheelDx += e.deltaX * unit;
      wheelDy += e.deltaY * unit;
      schedule();
    },
    { passive: false, signal },
  );

  // Suppress browser defaults (context menu, middle-click autoscroll, drag/select).
  stage.addEventListener("contextmenu", (e) => e.preventDefault(), { signal });
  canvas.addEventListener("mousedown", (e) => e.preventDefault(), { signal });
  canvas.addEventListener("auxclick", (e) => e.preventDefault(), { signal });
  canvas.addEventListener("dragstart", (e) => e.preventDefault(), { signal });

  const onKey = (down) => (e) => {
    if (!live || !e.code) return;
    // Keep toolbar buttons keyboard-accessible when they are focused.
    if (e.target instanceof HTMLElement && e.target.closest(".toolbar")) return;
    e.preventDefault();
    e.stopPropagation();
    send({ t: "key", code: e.code, down });
  };
  window.addEventListener("keydown", onKey(true), { capture: true, signal });
  window.addEventListener("keyup", onKey(false), { capture: true, signal });

  function releaseAll() {
    pressed.clear();
    send({ t: "release" });
  }
  window.addEventListener("blur", releaseAll, { signal });
  document.addEventListener(
    "visibilitychange",
    () => {
      if (document.visibilityState === "hidden") releaseAll();
    },
    { signal },
  );

  // ---- toolbar -----------------------------------------------------------

  async function toggleFullscreen() {
    try {
      if (document.fullscreenElement) {
        await document.exitFullscreen();
        return;
      }
      await root.requestFullscreen({ navigationUI: "hide" });
      try {
        await navigator.keyboard?.lock?.();
      } catch {
        // Keyboard Lock unsupported or denied; fullscreen still works.
      }
      canvas.focus({ preventScroll: true });
    } catch (err) {
      console.warn("fullscreen failed", err);
    }
  }

  document.addEventListener(
    "fullscreenchange",
    () => {
      const active = document.fullscreenElement === root;
      fullscreenBtn.textContent = active ? "Exit fullscreen" : "Fullscreen";
      if (!active) navigator.keyboard?.unlock?.();
    },
    { signal },
  );

  fullscreenBtn.addEventListener("click", toggleFullscreen, { signal });
  disconnectBtn.addEventListener("click", () => (location.hash = "#/"), { signal });
  reconnectBtn.addEventListener(
    "click",
    () => {
      if (!me) return route();
      connect();
    },
    { signal },
  );

  connect();

  cleanupView = () => {
    controller.abort();
    clearInterval(statsTimer);
    if (rafId) cancelAnimationFrame(rafId);
    video.reset();
    const socket = ws;
    ws = null;
    if (socket && socket.readyState <= WebSocket.OPEN) {
      if (socket.readyState === WebSocket.OPEN) socket.send('{"t":"release"}');
      socket.close(1000, "viewer closed");
    }
    navigator.keyboard?.unlock?.();
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    document.title = "doremote";
  };
}

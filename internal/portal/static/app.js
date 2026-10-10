/* shoal web portal: a small single-page app over the server's JSON API.
   Everything is built with DOM calls (never innerHTML), so names, messages
   and log lines can't inject markup. */
"use strict";

const $app = document.getElementById("app");

// ---------- DOM helpers ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "value") el.value = v;
    else if (v === true) el.setAttribute(k, "");
    else el.setAttribute(k, String(v));
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); }

// ---------- API ----------

class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(method, path, body, opts = {}) {
  const init = { method, credentials: "same-origin", headers: {} };
  if (method !== "GET") init.headers["X-Shoal-Request"] = "1";
  if (body !== undefined) {
    if (typeof body === "string") { init.body = body; init.headers["Content-Type"] = "text/plain"; }
    else { init.body = JSON.stringify(body); init.headers["Content-Type"] = "application/json"; }
  }
  const res = await fetch(path, init);
  if (res.status === 401 && !opts.allow401) { showLogin(); throw new ApiError(401, "not logged in"); }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return null;
  const type = res.headers.get("Content-Type") || "";
  return type.includes("json") ? res.json() : res.text();
}

const list = (kind) => api("GET", `/api/objects/${kind}`).then((r) => r.items || []);
const get = (kind, name) => api("GET", `/api/objects/${kind}/${encodeURIComponent(name)}`);
const del = (kind, name) => api("DELETE", `/api/objects/${kind}/${encodeURIComponent(name)}`);
const apply = (manifest) => api("POST", "/api/apply", manifest);

// ---------- Formatting ----------

function age(ts) {
  if (!ts) return "-";
  const s = Math.max(0, (Date.now() - Date.parse(ts)) / 1000);
  if (s < 120) return `${Math.floor(s)}s`;
  if (s < 7200) return `${Math.floor(s / 60)}m`;
  if (s < 172800) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

function memory(bytes) {
  let v = Number(bytes || 0);
  if (!v) return "-";
  const units = ["", "Ki", "Mi", "Gi", "Ti"];
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)}${units[i]}`;
}

function cpu(milli) {
  const m = Number(milli || 0);
  if (!m) return "-";
  return m % 1000 === 0 ? String(m / 1000) : `${m}m`;
}

const BADGE = {
  Ready: "ok", Running: "ok", Complete: "ok", Succeeded: "ok", yes: "ok",
  Progressing: "info", Pending: "info",
  NotReady: "bad", Failed: "bad", RolledBack: "warn", Stopped: "warn", no: "warn",
};

function badge(text, cls) {
  if (!text) return h("span", { class: "badge" }, "-");
  return h("span", { class: `badge ${cls || BADGE[text] || ""}` }, text);
}

function name(o) { return (o.metadata && o.metadata.name) || ""; }

function replicaReady(r) {
  const st = r.status || {}, spec = r.spec || {};
  return Boolean(st.ready) && String(st.templateGeneration || 0) === String(spec.templateGeneration || 0);
}

// toYAML renders plain data (objects, arrays, strings, numbers, booleans)
// as YAML for the editors. Strings are quoted whenever YAML could read them
// as something else.
function toYAML(v, indent = "") {
  const scalar = (x) => {
    if (typeof x === "number" || typeof x === "boolean") return String(x);
    const s = String(x);
    return /^[A-Za-z0-9_./][A-Za-z0-9_./ :@-]*$/.test(s) && !/^(true|false|null|yes|no|on|off|~|[-+]?[0-9.]+)$/i.test(s) &&
      !/: |\s$/.test(s) ? s : JSON.stringify(s);
  };
  if (Array.isArray(v)) {
    if (!v.length) return " []\n";
    return "\n" + v.map((item) => {
      if (item !== null && typeof item === "object") {
        const body = toYAML(item, indent + "  ").replace(/^\n/, "");
        return `${indent}- ${body.slice(indent.length + 2)}`;
      }
      return `${indent}- ${scalar(item)}\n`;
    }).join("");
  }
  if (v !== null && typeof v === "object") {
    const entries = Object.entries(v);
    if (!entries.length) return " {}\n";
    const out = entries.map(([k, x]) => (x !== null && typeof x === "object"
      ? `${indent}${k}:${toYAML(x, indent + "  ")}`
      : `${indent}${k}: ${scalar(x)}\n`)).join("");
    return indent ? "\n" + out : out;
  }
  return ` ${scalar(v)}\n`;
}

// ---------- Toasts and modals ----------

function toast(msg, bad) {
  let box = document.querySelector(".toasts");
  if (!box) { box = h("div", { class: "toasts" }); document.body.append(box); }
  const t = h("div", { class: `toast${bad ? " bad" : ""}` }, msg);
  box.append(t);
  setTimeout(() => t.remove(), bad ? 8000 : 4000);
}

function errorToast(err) { if (err.status !== 401) toast(err.message || String(err), true); }

let modalOpen = false;

function modal(title, body, actions) {
  const overlay = h("div", { class: "overlay" });
  const close = () => { overlay.remove(); modalOpen = false; };
  const footer = h("div", { class: "modal-footer" });
  for (const a of actions) {
    footer.append(h("button", {
      class: a.class || "",
      onclick: async (ev) => {
        if (!a.run) { close(); return; }
        ev.target.disabled = true;
        try { if ((await a.run()) !== false) close(); } catch (err) { errorToast(err); } finally { ev.target.disabled = false; }
      },
    }, a.label));
  }
  overlay.append(h("div", { class: "modal" }, h("div", { class: "modal-title" }, title), h("div", { class: "modal-body" }, body), footer));
  overlay.addEventListener("click", (ev) => { if (ev.target === overlay) close(); });
  document.body.append(overlay);
  modalOpen = true;
  const first = overlay.querySelector("input, textarea, select");
  if (first) first.focus();
  return close;
}

function confirmDelete(what, run) {
  modal(`Delete ${what}?`, h("p", {}, `This deletes ${what}. It can't be undone.`), [
    { label: "Cancel", class: "secondary" },
    { label: "Delete", class: "danger", run: async () => { await run(); toast(`Deleted ${what}`); render(); } },
  ]);
}

function field(label, input, help) {
  return h("div", { class: "field" }, h("label", {}, label), input, help ? h("div", { class: "help" }, help) : null);
}

function copyButton(text) {
  return h("button", {
    class: "secondary",
    onclick: async () => {
      try { await navigator.clipboard.writeText(text); toast("Copied"); } catch (_) { toast("Copy failed: select the text instead", true); }
    },
  }, "Copy");
}

// ---------- Tables ----------

function table(columns, rows, opts = {}) {
  if (!rows.length) return h("div", { class: "empty" }, opts.empty || "Nothing here yet.");
  return h("table", {},
    h("thead", {}, h("tr", {}, columns.map((c) => h("th", {}, c.title)))),
    h("tbody", {}, rows.map((row) => h("tr", {
      class: opts.onclick ? "clickable" : "",
      onclick: opts.onclick ? (ev) => { if (!ev.target.closest("button, a")) opts.onclick(row); } : null,
    }, columns.map((c) => h("td", { class: c.class || "" }, c.cell(row)))))));
}

// ---------- Layout and routing ----------

const NAV = [
  { section: "Cluster" },
  { path: "overview", label: "Overview" },
  { path: "nodes", label: "Nodes", kind: "nodes" },
  { section: "Applications" },
  { path: "workloads", label: "Workloads", kind: "workloads" },
  { path: "replicas", label: "Replicas", kind: "replicas" },
  { path: "services", label: "Services", kind: "services" },
  { path: "routes", label: "Routes", kind: "routes" },
  { section: "Configuration" },
  { path: "secrets", label: "Secrets", kind: "secrets" },
  { path: "configs", label: "Configs", kind: "configs" },
  { path: "apply", label: "Apply YAML" },
];

const ROUTES = [
  [/^overview$/, overviewPage],
  [/^nodes$/, nodesPage],
  [/^workloads$/, workloadsPage],
  [/^workloads\/new$/, newWorkloadPage],
  [/^workloads\/([^/]+)\/logs$/, logsPage],
  [/^workloads\/([^/]+)$/, workloadPage],
  [/^replicas$/, replicasPage],
  [/^services$/, servicesPage],
  [/^routes$/, routesPage],
  [/^secrets$/, (args) => dataListPage("secrets", args)],
  [/^secrets\/([^/]+)$/, (args) => dataPage("secrets", args)],
  [/^configs$/, (args) => dataListPage("configs", args)],
  [/^configs\/([^/]+)$/, (args) => dataPage("configs", args)],
  [/^apply$/, applyPage],
];

let me = null; // {server}
let refreshTimer = null;
let current = null; // {page, args}

function route() {
  const path = location.hash.replace(/^#\/?/, "") || "overview";
  for (const [re, page] of ROUTES) {
    const m = path.match(re);
    if (m) return { path, page, args: m.slice(1).map(decodeURIComponent) };
  }
  return { path: "overview", page: overviewPage, args: [] };
}

function layout(activePath) {
  const nav = h("nav", { class: "sidebar" }, NAV.map((n) => n.section
    ? h("div", { class: "section" }, n.section)
    : h("a", { href: `#/${n.path}`, class: activePath.split("/")[0] === n.path ? "active" : "" }, n.label)));
  const main = h("main", { class: "main" });
  const root = h("div", { class: "layout" },
    h("header", { class: "masthead" },
      h("img", { src: "logo.svg", alt: "" }),
      h("span", { class: "brand" }, "shoal"),
      h("span", { class: "cluster" }, me && me.server ? `cluster at ${me.server}` : ""),
      h("span", { class: "spacer" }),
      h("span", { class: "user" }, "admin"),
      h("button", { class: "secondary", onclick: logout }, "Log out")),
    nav, main);
  return { root, main };
}

// render draws the current route. Pages return {node, refresh}: refreshable
// pages are redrawn every few seconds unless the user is typing or a dialog
// is open.
async function render() {
  const r = route();
  current = r;
  const { root, main } = layout(r.path);
  clear($app);
  $app.append(root);
  await drawPage(main, r);
  clearInterval(refreshTimer);
  refreshTimer = setInterval(async () => {
    if (current !== r || modalOpen || !r.refreshable) return;
    const active = document.activeElement;
    if (active && main.contains(active) && ["INPUT", "TEXTAREA", "SELECT"].includes(active.tagName)) return;
    const scroll = main.scrollTop;
    await drawPage(main, r, true);
    main.scrollTop = scroll;
  }, 3000);
}

async function drawPage(main, r, quiet) {
  try {
    const out = await r.page(r.args);
    r.refreshable = out.refresh !== false;
    clear(main);
    main.append(out.node);
    if (out.mounted) out.mounted();
  } catch (err) {
    if (err.status === 401) return;
    if (quiet) return;
    clear(main);
    main.append(h("div", { class: "alert bad" }, h("strong", {}, "Couldn't load this page"), h("pre", {}, err.message)));
  }
}

function header(title, crumbs, ...actions) {
  return h("div", {},
    crumbs ? h("div", { class: "breadcrumb" }, crumbs) : null,
    h("div", { class: "page-header" }, h("h1", {}, title), h("span", { class: "spacer" }), h("div", { class: "btn-group" }, actions)));
}

// ---------- Login ----------

function showLogin() {
  clearInterval(refreshTimer);
  current = null;
  const pw = h("input", { type: "password", autocomplete: "current-password", placeholder: "Password" });
  const err = h("div");
  const submit = async (ev) => {
    ev.preventDefault();
    clear(err);
    try {
      await api("POST", "/api/login", { password: pw.value }, { allow401: true });
      await start();
    } catch (e) {
      err.append(h("div", { class: "alert bad" }, e.message));
      pw.select();
    }
  };
  clear($app);
  $app.append(h("div", { class: "login-wrap" },
    h("form", { class: "login", onsubmit: submit },
      h("div", { class: "logo" }, h("img", { src: "logo.svg", alt: "" }), h("span", {}, "shoal")),
      err,
      field("Admin password", pw),
      h("button", { type: "submit" }, "Log in"),
      h("div", { class: "help" },
        "The first password is in ", h("code", {}, "<data-dir>/ui-initial-password"), " on the server. ",
        "Set a new one with ", h("code", {}, "shoal ui reset-password"), ". ",
        "To stop browser certificate warnings, ", h("a", { href: "/ca.crt" }, "download the cluster CA"), " and trust it."))));
  pw.focus();
}

async function logout() {
  try { await api("POST", "/api/logout"); } catch (_) { /* logged out anyway */ }
  showLogin();
}

// ---------- Overview ----------

async function overviewPage() {
  const [nodes, workloads, replicas, services] = await Promise.all([list("nodes"), list("workloads"), list("replicas"), list("services")]);
  const readyNodes = nodes.filter((n) => (n.status || {}).phase === "Ready").length;
  const healthy = workloads.filter((w) => (w.status || {}).readyReplicas === (w.spec.replicas ?? 1) &&
    ((w.status || {}).rollout || {}).phase === "Complete").length;
  const running = replicas.filter(replicaReady).length;
  const stat = (label, value, total, cls) => h("div", { class: `stat ${cls}` }, h("div", { class: "label" }, label),
    h("div", { class: "value" }, value, total !== undefined ? h("small", {}, ` / ${total}`) : null));

  const attention = [];
  for (const n of nodes) if ((n.status || {}).phase !== "Ready") attention.push([badge("NotReady"), `Node ${name(n)} is not ready`, `#/nodes`]);
  for (const w of workloads) {
    const st = w.status || {}, ro = st.rollout || {};
    if (ro.phase && ro.phase !== "Complete") attention.push([badge(ro.phase), `Workload ${name(w)}: ${ro.message || ro.phase}`, `#/workloads/${name(w)}`]);
    else if ((st.readyReplicas || 0) < (w.spec.replicas ?? 1)) attention.push([badge("Pending"), `Workload ${name(w)}: ${st.readyReplicas || 0} of ${w.spec.replicas ?? 1} ready`, `#/workloads/${name(w)}`]);
  }
  for (const r of replicas) {
    const msg = (r.status || {}).message;
    if (msg && !replicaReady(r)) attention.push([badge((r.status || {}).phase || "Pending"), `Replica ${name(r)}: ${msg}`, `#/workloads/${r.spec.workload}`]);
  }

  return {
    node: h("div", {},
      header("Overview", null, h("button", { onclick: () => { location.hash = "#/workloads/new"; } }, "Create workload")),
      h("div", { class: "stats" },
        stat("Nodes ready", readyNodes, nodes.length, readyNodes === nodes.length && nodes.length ? "ok" : "bad"),
        stat("Healthy workloads", healthy, workloads.length, healthy === workloads.length ? "ok" : "warn"),
        stat("Ready replicas", running, replicas.length, running === replicas.length ? "ok" : "warn"),
        stat("Services", services.length)),
      h("div", { class: "grid2" },
        h("div", { class: "card" }, h("div", { class: "card-title" }, "Needs attention"),
          h("div", { class: "card-body flush" }, attention.length
            ? table([{ title: "", cell: (a) => a[0] }, { title: "", class: "msg", cell: (a) => h("a", { href: a[2] }, a[1]) }], attention.slice(0, 12))
            : h("div", { class: "empty" }, "All good: every node is ready and every workload is healthy."))),
        h("div", { class: "card" }, h("div", { class: "card-title" }, "Nodes"),
          h("div", { class: "card-body flush" }, nodesTable(nodes, true)))),
      h("div", { class: "card" }, h("div", { class: "card-title" }, "Workloads"),
        h("div", { class: "card-body flush" }, workloadsTable(workloads)))),
  };
}

// ---------- Nodes ----------

function nodesTable(nodes, compact) {
  const cols = [
    { title: "Name", cell: (n) => h("strong", {}, name(n)) },
    { title: "Status", cell: (n) => badge((n.status || {}).phase || "NotReady") },
    { title: "CPU", cell: (n) => cpu(((n.status || {}).capacity || {}).cpuMillis) },
    { title: "Memory", cell: (n) => memory(((n.status || {}).capacity || {}).memoryBytes) },
  ];
  if (!compact) {
    cols.push(
      { title: "Address", cell: (n) => (n.status || {}).address || "-" },
      { title: "Subnet", cell: (n) => (n.spec || {}).podCidr || "-" },
      { title: "Mesh", cell: (n) => ((n.status || {}).wireguardPublicKey ? badge("WireGuard", "ok") : "-") },
      { title: "Version", cell: (n) => (n.status || {}).agentVersion || "-" },
      { title: "Last heartbeat", cell: (n) => ((n.status || {}).lastHeartbeat ? `${age(n.status.lastHeartbeat)} ago` : "never") },
      { title: "Age", cell: (n) => age(n.metadata.createdAt) },
      { title: "", class: "actions", cell: (n) => h("button", { class: "link danger", onclick: () => confirmDelete(`node ${name(n)}`, () => del("nodes", name(n))) }, "Remove") });
  }
  return table(cols, nodes, { empty: "No nodes yet. Use “Add node” to get a join command." });
}

async function nodesPage() {
  const nodes = await list("nodes");
  return {
    node: h("div", {},
      header("Nodes", null, h("button", { onclick: addNodeDialog }, "Add node")),
      h("div", { class: "alert" }, "Removing a node moves its replicas to other nodes; its agent then needs a new join token."),
      h("div", { class: "card" }, h("div", { class: "card-body flush" }, nodesTable(nodes, false)))),
  };
}

function addNodeDialog() {
  const ttl = h("input", { type: "number", min: "1", max: "720", value: "24" });
  const uses = h("input", { type: "number", min: "0", value: "1" });
  const result = h("div");
  const body = h("div", {},
    h("p", {}, "Create a join token, then run the command on the new machine (Linux, with containerd and the CNI plugins installed)."),
    h("div", { class: "row" }, field("Valid for (hours)", ttl), field("Number of nodes (0 = unlimited)", uses)),
    result);
  modal("Add a node", body, [
    { label: "Close", class: "secondary" },
    {
      label: "Create join command", run: async () => {
        const res = await api("POST", "/api/tokens", { ttlHours: Number(ttl.value), maxUses: Number(uses.value) });
        clear(result);
        result.append(
          h("div", { class: "command" }, h("code", {}, res.command), copyButton(res.command)),
          h("div", { class: "help" }, `Valid until ${new Date(res.expiresAt).toLocaleString()}. Anyone with this command can add a node: keep it secret.`));
        return false; // keep the dialog open to copy the command
      },
    },
  ]);
}

// ---------- Workloads ----------

function rolloutBadge(w) {
  const ro = (w.status || {}).rollout || {};
  return badge(ro.phase || "Pending");
}

function workloadsTable(workloads) {
  return table([
    { title: "Name", cell: (w) => h("a", { href: `#/workloads/${name(w)}` }, h("strong", {}, name(w))) },
    { title: "Image", cell: (w) => h("span", { class: "mono" }, w.spec.image) },
    { title: "Ready", cell: (w) => `${(w.status || {}).readyReplicas || 0} / ${w.spec.replicas ?? 1}` },
    { title: "Up to date", cell: (w) => String((w.status || {}).updatedReplicas || 0) },
    { title: "Rollout", cell: rolloutBadge },
    { title: "Age", cell: (w) => age(w.metadata.createdAt) },
  ], workloads, { onclick: (w) => { location.hash = `#/workloads/${name(w)}`; }, empty: "No workloads yet." });
}

async function workloadsPage() {
  const workloads = await list("workloads");
  return {
    node: h("div", {},
      header("Workloads", null, h("button", { onclick: () => { location.hash = "#/workloads/new"; } }, "Create workload")),
      h("div", { class: "card" }, h("div", { class: "card-body flush" }, workloadsTable(workloads)))),
  };
}

function replicasTable(replicas, withWorkload) {
  const cols = [];
  if (withWorkload) cols.push({ title: "Workload", cell: (r) => h("a", { href: `#/workloads/${r.spec.workload}` }, r.spec.workload) });
  cols.push(
    { title: "Name", cell: (r) => h("span", { class: "mono" }, name(r)) },
    { title: "Node", cell: (r) => r.spec.node || "-" },
    { title: "Status", cell: (r) => badge((r.status || {}).phase || "Pending") },
    { title: "Ready", cell: (r) => badge(replicaReady(r) ? "yes" : "no") },
    { title: "Restarts", cell: (r) => String((r.status || {}).restarts || 0) },
    { title: "Generation", cell: (r) => String(r.spec.templateGeneration || "-") },
    { title: "IP", cell: (r) => (r.status || {}).ip || "-" },
    { title: "Age", cell: (r) => age(r.metadata.createdAt) },
    { title: "Message", class: "msg", cell: (r) => h("span", { title: (r.status || {}).message || "" }, (r.status || {}).message || "") },
    { title: "", class: "actions", cell: (r) => h("button", { class: "link danger", title: "The replica is replaced, possibly on another node", onclick: () => confirmDelete(`replica ${name(r)}`, () => del("replicas", name(r))) }, "Replace") });
  return table(cols, replicas, { empty: "No replicas." });
}

async function replicasPage() {
  const replicas = await list("replicas");
  return {
    node: h("div", {}, header("Replicas"),
      h("div", { class: "card" }, h("div", { class: "card-body flush" }, replicasTable(replicas, true)))),
  };
}

function describeHealth(hc) {
  if (!hc) return "none";
  const probe = hc.http ? `HTTP GET :${hc.http.port}${hc.http.path}` : `TCP :${(hc.tcp || {}).port}`;
  return `${probe}, every ${hc.intervalSeconds || 10}s, ${hc.failureThreshold || 3} failures to restart`;
}

async function workloadPage([wname]) {
  const [w, replicas, services] = await Promise.all([get("workloads", wname), list("replicas"), list("services")]);
  const st = w.status || {}, ro = st.rollout || {}, s = w.spec;
  const mine = replicas.filter((r) => r.spec.workload === wname);
  const svcs = services.filter((x) => x.spec.selector === wname);
  const want = s.replicas ?? 1;

  const scaleInput = h("input", { type: "number", min: "0", max: "1000", value: String(want), class: "narrow" });
  scaleInput.style.width = "90px";
  const doScale = async () => {
    try {
      await api("POST", `/api/workloads/${encodeURIComponent(wname)}/scale`, { replicas: Number(scaleInput.value) });
      toast(`Scaling ${wname} to ${scaleInput.value}`);
      render();
    } catch (err) { errorToast(err); }
  };
  const restart = () => modal(`Restart ${wname}?`, h("p", {}, "Replicas are replaced one batch at a time (rolling update), respecting maxUnavailable and health checks."), [
    { label: "Cancel", class: "secondary" },
    { label: "Restart", run: async () => { await api("POST", `/api/workloads/${encodeURIComponent(wname)}/restart`); toast(`Restarting ${wname}`); render(); } },
  ]);

  const env = (s.env || []).map((e) => {
    const from = e.valueFrom || {};
    if (from.secret) return h("div", {}, h("span", { class: "tag" }, e.name), " from secret ", h("a", { href: `#/secrets/${from.secret.name}` }, from.secret.name), ` / ${from.secret.key}`);
    if (from.config) return h("div", {}, h("span", { class: "tag" }, e.name), " from config ", h("a", { href: `#/configs/${from.config.name}` }, from.config.name), ` / ${from.config.key}`);
    return h("div", {}, h("span", { class: "tag" }, e.name), " = ", e.value || "");
  });
  const files = (s.files || []).map((f) => h("div", {}, h("span", { class: "tag" }, f.path), " ← ",
    f.secret ? ["secret ", h("a", { href: `#/secrets/${f.secret}` }, f.secret)] : ["config ", h("a", { href: `#/configs/${f.config}` }, f.config)]));
  const ports = (s.ports || []).map((p) => h("span", { class: "tag" }, `${p.hostPort ? p.hostPort + "->" : ""}${p.containerPort}/${p.protocol || "tcp"}`));

  return {
    node: h("div", {},
      header(wname, h("span", {}, h("a", { href: "#/workloads" }, "Workloads"), " / ", wname),
        h("a", { class: "btn", href: `#/workloads/${encodeURIComponent(wname)}/logs` }, "Logs"),
        h("button", { class: "secondary", onclick: () => editYamlDialog("workloads", wname) }, "Edit YAML"),
        h("button", { class: "secondary", onclick: restart }, "Restart"),
        h("button", { class: "danger", onclick: () => confirmDelete(`workload ${wname}`, async () => { await del("workloads", wname); location.hash = "#/workloads"; }) }, "Delete")),
      ro.message && ro.phase !== "Complete" ? h("div", { class: `alert ${ro.phase === "RolledBack" || ro.phase === "Failed" ? "bad" : ""}` }, h("strong", {}, `Rollout ${ro.phase}: `), ro.message) : null,
      h("div", { class: "grid2" },
        h("div", { class: "card" }, h("div", { class: "card-title" }, "Status"),
          h("div", { class: "card-body" }, h("dl", { class: "props" },
            h("dt", {}, "Rollout"), h("dd", {}, rolloutBadge(w)),
            h("dt", {}, "Ready"), h("dd", {}, `${st.readyReplicas || 0} of ${want}`),
            h("dt", {}, "Up to date"), h("dd", {}, String(st.updatedReplicas || 0)),
            h("dt", {}, "Generation"), h("dd", {}, `${w.metadata.generation} (observed ${st.observedGeneration || 0})`),
            h("dt", {}, "Stable version"), h("dd", {}, ro.stableGeneration ? `generation ${ro.stableGeneration} (${(ro.stableTemplate || {}).image || ""})` : "-"),
            h("dt", {}, "Scale"), h("dd", {}, h("div", { class: "row" }, scaleInput, h("button", { class: "secondary narrow", onclick: doScale }, "Apply")))))),
        h("div", { class: "card" }, h("div", { class: "card-title" }, "Spec"),
          h("div", { class: "card-body" }, h("dl", { class: "props" },
            h("dt", {}, "Image"), h("dd", { class: "mono" }, s.image),
            s.command ? [h("dt", {}, "Command"), h("dd", { class: "mono" }, s.command.join(" "))] : null,
            h("dt", {}, "Ports"), h("dd", {}, ports.length ? ports : "-"),
            h("dt", {}, "Resources"), h("dd", {}, `cpu ${(s.resources || {}).cpu || "-"}, memory ${(s.resources || {}).memory || "-"}`),
            h("dt", {}, "Health check"), h("dd", {}, describeHealth(s.healthCheck)),
            h("dt", {}, "Update"), h("dd", {}, `maxUnavailable ${(s.update || {}).maxUnavailable ?? 1}, deadline ${(s.update || {}).progressDeadlineSeconds || 120}s`),
            h("dt", {}, "Restart policy"), h("dd", {}, s.restartPolicy || "Always"),
            h("dt", {}, "Environment"), h("dd", {}, env.length ? env : "-"),
            h("dt", {}, "Files"), h("dd", {}, files.length ? files : "-"),
            h("dt", {}, "Services"), h("dd", {}, svcs.length ? svcs.map((x) => h("span", { class: "tag" }, `${name(x)}.shoal (${(x.status || {}).clusterIp || "pending"})`)) : "-"))))),
      h("div", { class: "card" }, h("div", { class: "card-title" }, `Replicas (${mine.length})`),
        h("div", { class: "card-body flush" }, replicasTable(mine, false)))),
  };
}

// editYamlDialog edits an object as YAML and applies it.
async function editYamlDialog(kind, oname) {
  let text;
  try { text = await api("GET", `/api/objects/${kind}/${encodeURIComponent(oname)}/yaml`); } catch (err) { errorToast(err); return; }
  const area = h("textarea", {}, text);
  modal(`Edit ${oname}`, h("div", {}, h("p", { class: "help" }, "Changes are applied like `shoal apply`; workload changes roll out gradually."), area), [
    { label: "Cancel", class: "secondary" },
    { label: "Apply", run: async () => { const r = await apply(area.value); toast(resultText(r)); render(); } },
  ]);
}

function resultText(r) {
  return (r.results || []).map((x) => `${x.kind.toLowerCase()}/${x.name} ${x.action}`).join(", ") || "applied";
}

// ---------- Create workload ----------

async function newWorkloadPage() {
  const [secrets, configs] = await Promise.all([list("secrets"), list("configs")]);
  const inp = (attrs) => h("input", Object.assign({ type: "text" }, attrs));
  const f = {
    name: inp({ placeholder: "web" }), image: inp({ placeholder: "nginx:1.28-alpine" }),
    replicas: inp({ type: "number", value: "2", min: "0" }), port: inp({ type: "number", placeholder: "80" }),
    hostPort: inp({ type: "number", placeholder: "none" }),
    cpu: inp({ placeholder: "200m" }), memory: inp({ placeholder: "128Mi" }),
    restart: h("select", {}, ["Always", "OnFailure", "Never"].map((p) => h("option", {}, p))),
    command: inp({ placeholder: "optional, e.g. sleep 3600" }),
    hcType: h("select", {}, h("option", { value: "" }, "none"), h("option", { value: "http" }, "HTTP"), h("option", { value: "tcp" }, "TCP")),
    hcPath: inp({ value: "/" }), hcPort: inp({ type: "number", placeholder: "same as container port" }),
    hcInterval: inp({ type: "number", value: "10" }),
    maxUnavail: inp({ type: "number", value: "1", min: "1" }),
    service: h("input", { type: "checkbox" }),
  };
  const envRows = h("div");
  const fileRows = h("div");
  const optionsFor = (kind) => (kind === "secret" ? secrets : configs).map((o) => h("option", {}, name(o)));

  const addEnv = () => {
    const ename = inp({ placeholder: "NAME" }), type = h("select", {}, h("option", { value: "value" }, "value"), h("option", { value: "secret" }, "from secret"), h("option", { value: "config" }, "from config"));
    const value = inp({ placeholder: "value" }), src = h("select", {}), key = inp({ placeholder: "key" });
    const sync = () => {
      const ref = type.value !== "value";
      value.style.display = ref ? "none" : ""; src.style.display = ref ? "" : "none"; key.style.display = ref ? "" : "none";
      clear(src); if (ref) append(src, [optionsFor(type.value)]);
    };
    type.addEventListener("change", sync);
    const row = h("div", { class: "row field" }, ename, type, value, src, key, h("button", { class: "link danger narrow", onclick: () => row.remove() }, "Remove"));
    row.collect = () => {
      if (!ename.value) return null;
      if (type.value === "value") return { name: ename.value, value: value.value };
      return { name: ename.value, valueFrom: { [type.value]: { name: src.value, key: key.value } } };
    };
    envRows.append(row); sync();
  };
  const addFile = () => {
    const path = inp({ placeholder: "/etc/app" }), type = h("select", {}, h("option", { value: "config" }, "config"), h("option", { value: "secret" }, "secret"));
    const src = h("select", {});
    const sync = () => { clear(src); append(src, [optionsFor(type.value)]); };
    type.addEventListener("change", sync);
    const row = h("div", { class: "row field" }, path, type, src, h("button", { class: "link danger narrow", onclick: () => row.remove() }, "Remove"));
    row.collect = () => (path.value ? { path: path.value, [type.value]: src.value } : null);
    fileRows.append(row); sync();
  };

  const manifest = () => {
    const spec = { image: f.image.value.trim(), replicas: Number(f.replicas.value || 1), restartPolicy: f.restart.value };
    if (f.command.value.trim()) spec.command = f.command.value.trim().split(/\s+/);
    if (f.port.value) {
      const port = { containerPort: Number(f.port.value) };
      if (f.hostPort.value) port.hostPort = Number(f.hostPort.value);
      spec.ports = [port];
    }
    if (f.cpu.value || f.memory.value) spec.resources = Object.fromEntries([["cpu", f.cpu.value], ["memory", f.memory.value]].filter((x) => x[1]));
    const env = [...envRows.children].map((r) => r.collect()).filter(Boolean);
    if (env.length) spec.env = env;
    const files = [...fileRows.children].map((r) => r.collect()).filter(Boolean);
    if (files.length) spec.files = files;
    if (f.hcType.value) {
      const port = Number(f.hcPort.value || f.port.value || 80);
      spec.healthCheck = { intervalSeconds: Number(f.hcInterval.value || 10) };
      spec.healthCheck[f.hcType.value] = f.hcType.value === "http" ? { path: f.hcPath.value || "/", port } : { port };
    }
    spec.update = { maxUnavailable: Number(f.maxUnavail.value || 1) };
    const docs = [{ apiVersion: "v1alpha1", kind: "Workload", metadata: { name: f.name.value.trim() }, spec }];
    if (f.service.checked && f.port.value) {
      docs.push({ apiVersion: "v1alpha1", kind: "Service", metadata: { name: f.name.value.trim() }, spec: { selector: f.name.value.trim(), ports: [{ port: Number(f.port.value) }] } });
    }
    return docs.map((d) => toYAML(d)).join("---\n"); // not map(toYAML): map passes the index as indent
  };

  const yamlArea = h("textarea", {});
  const formView = h("div", {},
    h("div", { class: "grid2" },
      h("div", {},
        h("fieldset", {}, h("legend", {}, "Container"),
          field("Name", f.name, "Lowercase letters, digits and '-'."),
          field("Image", f.image),
          field("Command", f.command),
          h("div", { class: "row" }, field("Replicas", f.replicas), field("Restart policy", f.restart))),
        h("fieldset", {}, h("legend", {}, "Network"),
          h("div", { class: "row" }, field("Container port", f.port), field("Host port", f.hostPort)),
          h("label", { class: "help" }, f.service, " Also create a Service (stable IP and DNS name <name>.shoal)"))),
      h("div", {},
        h("fieldset", {}, h("legend", {}, "Resources"),
          h("div", { class: "row" }, field("CPU", f.cpu, "Per replica, e.g. 200m or 1."), field("Memory", f.memory, "e.g. 128Mi or 1Gi."))),
        h("fieldset", {}, h("legend", {}, "Health check and updates"),
          h("div", { class: "row" }, field("Health check", f.hcType), field("HTTP path", f.hcPath)),
          h("div", { class: "row" }, field("Port", f.hcPort), field("Interval (s)", f.hcInterval)),
          field("Max unavailable during updates", f.maxUnavail)))),
    h("fieldset", {}, h("legend", {}, "Environment variables"), envRows, h("button", { class: "secondary", onclick: addEnv }, "Add variable")),
    h("fieldset", {}, h("legend", {}, "Files from secrets and configs"), fileRows, h("button", { class: "secondary", onclick: addFile }, "Add files")));
  const yamlView = h("div", { class: "hidden" }, h("p", { class: "help" }, "Any manifest that `shoal apply -f` accepts (YAML or JSON, several documents separated by ---)."), yamlArea);

  const tabForm = h("button", { class: "active" }, "Form");
  const tabYaml = h("button", {}, "YAML");
  // Toggled with a class: the CSP forbids inline style attributes.
  tabForm.addEventListener("click", () => { formView.classList.remove("hidden"); yamlView.classList.add("hidden"); tabForm.className = "active"; tabYaml.className = ""; });
  tabYaml.addEventListener("click", () => { yamlArea.value = yamlArea.value || manifest(); formView.classList.add("hidden"); yamlView.classList.remove("hidden"); tabYaml.className = "active"; tabForm.className = ""; });

  const create = async () => {
    const text = yamlView.classList.contains("hidden") ? manifest() : yamlArea.value;
    try {
      const r = await apply(text);
      toast(resultText(r));
      const w = (r.results || []).find((x) => x.kind === "Workload");
      location.hash = w ? `#/workloads/${w.name}` : "#/workloads";
    } catch (err) { errorToast(err); }
  };
  return {
    refresh: false,
    node: h("div", {},
      header("Create workload", h("span", {}, h("a", { href: "#/workloads" }, "Workloads"), " / new"),
        h("button", { class: "secondary", onclick: () => { location.hash = "#/workloads"; } }, "Cancel"),
        h("button", { onclick: create }, "Create")),
      h("div", { class: "card" }, h("div", { class: "card-body" }, h("div", { class: "tabs" }, tabForm, tabYaml), formView, yamlView))),
    mounted: () => f.name.focus(),
  };
}

// ---------- Logs ----------

async function logsPage([wname]) {
  const replicas = (await list("replicas")).filter((r) => r.spec.workload === wname);
  const out = h("div", { class: "logs" });
  const which = h("select", {}, h("option", { value: "" }, "All replicas"), replicas.map((r) => h("option", { value: name(r) }, name(r))));
  const tail = h("select", {}, ["100", "500", "2000"].map((n) => h("option", { value: n }, `last ${n}`)), h("option", { value: "-1" }, "everything"));
  const follow = h("input", { type: "checkbox", checked: true });
  const stamps = h("input", { type: "checkbox" });
  const state = h("span", { class: "help" });
  let source = null;

  const start = () => {
    if (source) source.close();
    clear(out);
    const q = new URLSearchParams({ workload: wname, tail: tail.value, follow: follow.checked ? "1" : "0" });
    if (which.value) q.set("replica", which.value);
    source = new EventSource(`/api/logs?${q}`);
    state.textContent = follow.checked ? "following…" : "loading…";
    source.onmessage = (ev) => {
      const l = JSON.parse(ev.data);
      const atBottom = out.scrollHeight - out.scrollTop - out.clientHeight < 40;
      const line = h("div", { class: l.stream === "stderr" ? "err" : l.stream === "shoal" ? "note" : "" },
        which.value ? null : h("span", { class: "rep" }, `[${l.replica}] `),
        stamps.checked && l.time ? h("span", { class: "ts" }, `${l.time} `) : null,
        l.line);
      out.append(line);
      while (out.childElementCount > 5000) out.firstChild.remove();
      if (atBottom) out.scrollTop = out.scrollHeight;
    };
    source.addEventListener("failure", (ev) => { out.append(h("div", { class: "note" }, ev.data)); });
    source.addEventListener("end", () => { source.close(); state.textContent = "end of logs"; });
    source.onerror = () => { state.textContent = "disconnected"; source.close(); };
  };
  for (const c of [which, tail, follow, stamps]) c.addEventListener("change", start);
  // Stop streaming when the user leaves the page.
  window.addEventListener("hashchange", () => source && source.close(), { once: true });

  return {
    refresh: false,
    node: h("div", {},
      header(`Logs: ${wname}`, h("span", {}, h("a", { href: "#/workloads" }, "Workloads"), " / ", h("a", { href: `#/workloads/${wname}` }, wname), " / logs")),
      h("div", { class: "card" },
        h("div", { class: "log-toolbar" }, which, tail, h("label", {}, follow, "Follow"), h("label", {}, stamps, "Timestamps"),
          h("button", { class: "secondary", onclick: start }, "Reload"), state),
        out)),
    mounted: start,
  };
}

// ---------- Services ----------

async function servicesPage() {
  const [services, workloads] = await Promise.all([list("services"), list("workloads")]);
  const create = () => {
    const sname = h("input", { type: "text", placeholder: "web" });
    const sel = h("select", {}, workloads.map((w) => h("option", {}, name(w))));
    const port = h("input", { type: "number", placeholder: "80" });
    const target = h("input", { type: "number", placeholder: "same as port" });
    const proto = h("select", {}, h("option", {}, "tcp"), h("option", {}, "udp"));
    modal("Create service", h("div", {},
      h("p", { class: "help" }, "The service gets a virtual IP and the DNS name <name>.shoal, reachable from containers on every node, and spreads connections over the workload's ready replicas."),
      field("Name", sname), field("Workload", sel),
      h("div", { class: "row" }, field("Port", port), field("Target port", target), field("Protocol", proto))), [
      { label: "Cancel", class: "secondary" },
      {
        label: "Create", run: async () => {
          const p = { port: Number(port.value), protocol: proto.value };
          if (target.value) p.targetPort = Number(target.value);
          const r = await apply(JSON.stringify({ kind: "Service", metadata: { name: sname.value.trim() }, spec: { selector: sel.value, ports: [p] } }));
          toast(resultText(r)); render();
        },
      },
    ]);
  };
  return {
    node: h("div", {},
      header("Services", null, h("button", { onclick: create, disabled: !workloads.length }, "Create service")),
      h("div", { class: "card" }, h("div", { class: "card-body flush" }, table([
        { title: "Name", cell: (s) => h("strong", {}, name(s)) },
        { title: "DNS name", cell: (s) => h("span", { class: "mono" }, `${name(s)}.shoal`) },
        { title: "Cluster IP", cell: (s) => (s.status || {}).clusterIp || "pending" },
        { title: "Ports", cell: (s) => (s.spec.ports || []).map((p) => h("span", { class: "tag" }, `${p.port}->${p.targetPort}/${p.protocol}`)) },
        { title: "Workload", cell: (s) => h("a", { href: `#/workloads/${s.spec.selector}` }, s.spec.selector) },
        { title: "Endpoints", cell: (s) => { const n = ((s.status || {}).endpoints || []).length; return badge(String(n), n ? "ok" : "bad"); } },
        { title: "Age", cell: (s) => age(s.metadata.createdAt) },
        { title: "", class: "actions", cell: (s) => h("button", { class: "link danger", onclick: () => confirmDelete(`service ${name(s)}`, () => del("services", name(s))) }, "Delete") },
      ], services, { empty: "No services yet." })))),
  };
}

// ---------- Routes ----------

// Read-only list with delete. Routes are created with Apply YAML.
async function routesPage() {
  const routes = await list("routes");
  return {
    node: h("div", {},
      header("Routes", null),
      h("div", { class: "card" }, h("div", { class: "card-body flush" }, table([
        { title: "Name", cell: (r) => h("strong", {}, name(r)) },
        { title: "Host", cell: (r) => h("span", { class: "mono" }, r.spec.host) },
        { title: "Path", cell: (r) => h("span", { class: "mono" }, r.spec.pathPrefix || "/") },
        { title: "Service", cell: (r) => h("a", { href: "#/services" }, r.spec.service) },
        { title: "Port", cell: (r) => String(r.spec.port) },
        { title: "TLS", cell: (r) => r.spec.tls || "auto" },
        { title: "Age", cell: (r) => age(r.metadata.createdAt) },
        { title: "", class: "actions", cell: (r) => h("button", { class: "link danger", onclick: () => confirmDelete(`route ${name(r)}`, () => del("routes", name(r))) }, "Delete") },
      ], routes, { empty: "No routes yet. Create one with Apply YAML." })))),
  };
}

// ---------- Secrets and configs ----------

const DATA = {
  secrets: { title: "Secrets", one: "secret", hidden: true, help: "Encrypted at rest. Values are never shown again once saved; you can only replace them." },
  configs: { title: "Configs", one: "config", hidden: false, help: "Plain configuration, mounted as files or used as environment variables." },
};

function keyValueRows(hidden) {
  const box = h("div");
  const add = (k = "", v = "") => {
    const key = h("input", { type: "text", placeholder: "key", value: k });
    const val = hidden ? h("input", { type: "password", placeholder: "value", value: v, autocomplete: "new-password" }) : h("textarea", { class: "small", placeholder: "value" }, v);
    const row = h("div", { class: "row field" }, key, val, h("button", { class: "link danger narrow", onclick: () => row.remove() }, "Remove"));
    row.collect = () => (key.value ? [key.value.trim(), val.value] : null);
    box.append(row);
  };
  return { box, add, collect: () => Object.fromEntries([...box.children].map((r) => r.collect()).filter(Boolean)) };
}

async function dataListPage(kind) {
  const d = DATA[kind];
  const items = await list(kind);
  const create = () => {
    const nm = h("input", { type: "text", placeholder: "db" });
    const kv = keyValueRows(d.hidden);
    kv.add();
    modal(`Create ${d.one}`, h("div", {}, h("p", { class: "help" }, d.help), field("Name", nm), h("label", {}, "Data"), kv.box,
      h("button", { class: "secondary", onclick: () => kv.add() }, "Add key")), [
      { label: "Cancel", class: "secondary" },
      {
        label: "Create", run: async () => {
          await api("PATCH", `/api/data/${kind}/${encodeURIComponent(nm.value.trim())}`, { set: kv.collect() });
          toast(`${d.one} ${nm.value} saved`); location.hash = `#/${kind}/${nm.value.trim()}`;
        },
      },
    ]);
  };
  return {
    node: h("div", {},
      header(d.title, null, h("button", { onclick: create }, `Create ${d.one}`)),
      h("div", { class: "alert" }, d.help),
      h("div", { class: "card" }, h("div", { class: "card-body flush" }, table([
        { title: "Name", cell: (o) => h("a", { href: `#/${kind}/${name(o)}` }, h("strong", {}, name(o))) },
        { title: "Keys", cell: (o) => Object.keys((o.spec || {}).data || {}).sort().map((k) => h("span", { class: "tag" }, k)) },
        { title: "Age", cell: (o) => age(o.metadata.createdAt) },
        { title: "", class: "actions", cell: (o) => h("button", { class: "link danger", onclick: () => confirmDelete(`${d.one} ${name(o)}`, () => del(kind, name(o))) }, "Delete") },
      ], items, { onclick: (o) => { location.hash = `#/${kind}/${name(o)}`; }, empty: `No ${kind} yet.` })))),
  };
}

async function dataPage(kind, [oname]) {
  const d = DATA[kind];
  const [o, workloads] = await Promise.all([get(kind, oname), list("workloads")]);
  const data = (o.spec || {}).data || {};
  const users = workloads.filter((w) => JSON.stringify(w.spec).includes(`"${oname}"`));
  const patch = async (body, msg) => {
    try { await api("PATCH", `/api/data/${kind}/${encodeURIComponent(oname)}`, body); toast(msg); render(); } catch (err) { errorToast(err); }
  };
  const editKey = (k) => {
    const val = d.hidden ? h("input", { type: "password", autocomplete: "new-password" }) : h("textarea", {}, data[k] || "");
    modal(`${d.hidden ? "Replace" : "Edit"} ${k}`, h("div", {}, field("Value", val, d.hidden ? "The current value is not shown." : null)), [
      { label: "Cancel", class: "secondary" },
      { label: "Save", run: () => patch({ set: { [k]: val.value } }, `${k} saved`) },
    ]);
  };
  const addKeys = () => {
    const kv = keyValueRows(d.hidden);
    kv.add();
    modal("Add keys", h("div", {}, kv.box, h("button", { class: "secondary", onclick: () => kv.add() }, "Add key")), [
      { label: "Cancel", class: "secondary" },
      { label: "Save", run: () => patch({ set: kv.collect() }, "keys saved") },
    ]);
  };
  return {
    node: h("div", {},
      header(oname, h("span", {}, h("a", { href: `#/${kind}` }, d.title), " / ", oname),
        h("button", { class: "secondary", onclick: addKeys }, "Add keys"),
        d.hidden ? null : h("button", { class: "secondary", onclick: () => editYamlDialog(kind, oname) }, "Edit YAML"),
        h("button", { class: "danger", onclick: () => confirmDelete(`${d.one} ${oname}`, async () => { await del(kind, oname); location.hash = `#/${kind}`; }) }, "Delete")),
      h("div", { class: "alert" }, "Changed values reach mounted files within seconds; environment variables pick them up when the workload restarts."),
      h("div", { class: "card" }, h("div", { class: "card-title" }, "Data"), h("div", { class: "card-body flush" }, table([
        { title: "Key", cell: ([k]) => h("span", { class: "mono" }, k) },
        { title: "Value", class: "msg", cell: ([, v]) => (d.hidden ? h("span", { class: "help" }, "•••••• (hidden)") : h("span", { class: "mono", title: v }, v)) },
        {
          title: "", class: "actions", cell: ([k]) => [
            h("button", { class: "link", onclick: () => editKey(k) }, d.hidden ? "Replace" : "Edit"),
            h("button", { class: "link danger", onclick: () => patch({ remove: [k] }, `${k} removed`) }, "Remove")],
        },
      ], Object.entries(data).sort(), { empty: "No keys." }))),
      h("div", { class: "card" }, h("div", { class: "card-title" }, "Used by"), h("div", { class: "card-body" },
        users.length ? users.map((w) => h("div", {}, h("a", { href: `#/workloads/${name(w)}` }, name(w)))) : h("span", { class: "help" }, "No workload uses it.")))),
  };
}

// ---------- Apply ----------

async function applyPage() {
  const area = h("textarea", { placeholder: "kind: Workload\nmetadata: { name: web }\nspec:\n  image: nginx:1.28-alpine\n  replicas: 2" });
  const out = h("div");
  const run = async () => {
    clear(out);
    try {
      const r = await apply(area.value);
      out.append(h("div", { class: "alert ok" }, resultText(r)));
    } catch (err) {
      if (err.status !== 401) out.append(h("div", { class: "alert bad" }, h("strong", {}, "Not applied"), h("pre", {}, err.message)));
    }
  };
  return {
    refresh: false,
    node: h("div", {},
      header("Apply YAML", null, h("button", { onclick: run }, "Apply")),
      out,
      h("div", { class: "card" }, h("div", { class: "card-body" },
        h("p", { class: "help" }, "Paste one or more objects (Workload, Service, Secret, Config), separated by ---. Everything is validated before anything is changed."),
        area))),
    mounted: () => area.focus(),
  };
}

// ---------- Start ----------

async function start() {
  try {
    me = await api("GET", "/api/me", undefined, { allow401: true });
  } catch (_) { me = null; }
  if (!me || !me.authenticated) { showLogin(); return; }
  await render();
}

window.addEventListener("hashchange", () => { if (me && me.authenticated) render(); });
start();

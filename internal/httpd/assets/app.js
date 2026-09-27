/* tidymail front end.
 *
 * Runs under a strict CSP with require-trusted-types-for 'script', so this file
 * never assigns to innerHTML: every piece of mail content reaches the page as a
 * text node. That is the whole XSS story — a hostile message body is inert by
 * construction, not by sanitising.
 */
"use strict";

const state = {
  csrf: "",
  boxes: [],
  fromAddr: "",
  accounts: [], // what a new message can be sent from
  files: { newmail: [], compose: [] }, // attachments picked per form
  box: "unprocessed",
  query: "",
  unreadOnly: false,
  offset: 0,
  limit: 50,
  total: 0,
  items: [],
  selected: -1,
  message: null,
  checked: new Set(), // ids ticked for a bulk action
  allResults: false, // the whole query is selected, not just the ticked ids
};

const el = (id) => document.getElementById(id);

/* ---------- server calls ------------------------------------------------- */

async function api(path, options = {}) {
  const opts = { credentials: "same-origin", headers: {}, ...options };
  if (opts.form !== undefined) {
    // Multipart upload: the browser sets the Content-Type with its boundary.
    opts.method = opts.method || "POST";
    opts.body = opts.form;
    delete opts.form;
  } else if (opts.body !== undefined) {
    opts.method = opts.method || "POST";
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(opts.body);
  }
  if (opts.method && opts.method !== "GET") {
    opts.headers["X-CSRF-Token"] = state.csrf;
  }

  let res;
  try {
    res = await fetch(path, opts);
  } catch (err) {
    throw new Error(`cannot reach tidymail: ${err.message}`);
  }
  const text = await res.text();
  let body = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch (err) {
      throw new Error(`unexpected reply from ${path}: ${err.message}`);
    }
  }
  if (!res.ok) {
    throw new Error((body && body.error) || `${path} failed with HTTP ${res.status}`);
  }
  return body;
}

/* ---------- small helpers ------------------------------------------------ */

function toast(message, kind) {
  const node = document.createElement("div");
  node.className = kind ? `toast ${kind}` : "toast";
  node.textContent = message;
  el("toasts").append(node);
  window.setTimeout(() => node.remove(), kind === "error" ? 9000 : 5000);
}

function report(err) {
  console.error(err);
  toast(err.message || String(err), "error");
}

function humanDate(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  if (sameDay) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  if (d.getFullYear() === now.getFullYear()) {
    return d.toLocaleDateString(undefined, { day: "numeric", month: "short" });
  }
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

function humanSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} kB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function displayName(item) {
  return item.from_name || item.from_addr || "(no sender)";
}

function boxName(slug) {
  const box = state.boxes.find((b) => b.slug === slug);
  return box ? box.name : slug;
}

function clear(node) {
  while (node.firstChild) node.firstChild.remove();
}

function span(className, text) {
  const s = document.createElement("span");
  if (className) s.className = className;
  s.textContent = text;
  return s;
}

/* ---------- sidebar ------------------------------------------------------ */

async function loadState() {
  const data = await api("/api/state");
  state.csrf = data.csrf;
  state.boxes = data.boxes;
  state.fromAddr = data.from_addr;
  state.accounts = data.accounts || [];
  renderBoxes();
  renderStats(data.stats);
}

function renderBoxes() {
  const list = el("boxes");
  clear(list);
  for (const box of state.boxes) {
    const li = document.createElement("li");

    const button = document.createElement("button");
    button.type = "button";
    button.className = "box";
    button.setAttribute("aria-current", String(box.slug === state.box));
    button.append(span("box-name", box.name));
    const count = span(box.unread > 0 ? "count unread" : "count", box.unread > 0 ? `${box.unread}/${box.total}` : String(box.total));
    button.append(count);
    button.addEventListener("click", () => selectBox(box.slug));
    li.append(button);

    if (!box.system) {
      const drop = document.createElement("button");
      drop.type = "button";
      drop.className = "drop";
      drop.title = `Delete the ${box.name} box`;
      drop.textContent = "×";
      drop.addEventListener("click", (ev) => {
        ev.stopPropagation();
        confirmDeleteBox(box);
      });
      li.append(drop);
    }
    list.append(li);
  }
}

function renderStats(stats) {
  const last = stats.last_sync && !stats.last_sync.startsWith("0001")
    ? new Date(stats.last_sync).toLocaleString()
    : "never";
  const pending = stats.pending_imap ? `\nimap moves pending: ${stats.pending_imap}` : "";
  el("stats").textContent =
    `${stats.messages} messages\n${stats.contacts} whitelisted\n${stats.rules} rules\nsynced: ${last}${pending}`;
}

function selectBox(slug) {
  state.box = slug;
  state.offset = 0;
  state.selected = -1;
  clearChecked();
  renderBoxes();
  loadList().catch(report);
}

/* ---------- message list ------------------------------------------------- */

async function loadList(keepSelection = false) {
  const params = new URLSearchParams({
    box: state.query ? "" : state.box,
    q: state.query,
    limit: String(state.limit),
    offset: String(state.offset),
  });
  if (state.unreadOnly) params.set("unread", "1");

  const data = await api(`/api/messages?${params.toString()}`);
  state.items = data.items;
  state.total = data.total;
  if (!keepSelection) state.selected = -1;
  renderList();
}

function renderList() {
  el("list-head").textContent = state.query
    ? `search "${state.query}" — ${state.total} match(es) across all boxes`
    : `${boxName(state.box)} — ${state.total} message(s)`;

  const list = el("messages");
  clear(list);
  const checkable = bulkAllowed();
  list.classList.toggle("checkable", checkable);

  state.items.forEach((item, i) => {
    const li = document.createElement("li");
    li.tabIndex = 0;
    li.setAttribute("aria-selected", String(i === state.selected));
    if (item.unread) li.classList.add("unread");

    if (checkable) {
      const box = document.createElement("input");
      box.type = "checkbox";
      box.className = "row-check";
      box.checked = state.allResults || state.checked.has(item.id);
      box.setAttribute("aria-label", "Select message");
      box.addEventListener("click", (ev) => {
        ev.stopPropagation();
        toggleChecked(item.id, box.checked);
      });
      li.append(box);
    }

    const top = document.createElement("div");
    top.className = "row-top";
    top.append(span("row-from", displayName(item)));
    top.append(span("row-date", humanDate(item.date)));
    li.append(top);

    const subject = span("row-subject", item.subject || "(no subject)");
    if (item.attachments > 0) {
      subject.append(document.createTextNode(" "));
      subject.append(span("tag clip", String(item.attachments)));
    }
    if (state.query || item.box !== state.box) {
      subject.append(document.createTextNode(" "));
      subject.append(span("tag", boxName(item.box)));
    }
    if (item.delayed_until) {
      subject.append(document.createTextNode(" "));
      subject.append(span("tag", `until ${humanDate(item.delayed_until)}`));
    }
    li.append(subject);

    if (item.snippet) li.append(span("row-snippet", item.snippet));

    li.addEventListener("click", () => openIndex(i));
    li.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter") openIndex(i);
    });
    list.append(li);
  });

  const page = Math.floor(state.offset / state.limit) + 1;
  const pages = Math.max(1, Math.ceil(state.total / state.limit));
  el("page-info").textContent = `page ${page} of ${pages}`;
  el("prev-page").disabled = state.offset === 0;
  el("next-page").disabled = state.offset + state.limit >= state.total;
  el("archive-all").hidden = !(state.box === "unprocessed" && !state.query && state.total > 0);
  el("empty-trash").hidden = !(state.box === "trash" && !state.query && state.total > 0);
  renderBulk();
}

/* ---------- bulk selection ----------------------------------------------- */

// bulkAllowed: every list can tick messages. In Trash itself the bulk action
// deletes for good instead of moving to Trash.
function bulkAllowed() {
  return true;
}

// inTrash: the list is the Trash box, not a search across boxes.
function inTrash() {
  return state.box === "trash" && !state.query;
}

function clearChecked() {
  state.checked.clear();
  state.allResults = false;
}

function toggleChecked(id, on) {
  if (state.allResults) {
    // Unticking one row turns "all results" back into this page's ticks.
    state.allResults = false;
    for (const it of state.items) state.checked.add(it.id);
  }
  if (on) state.checked.add(id);
  else state.checked.delete(id);
  renderList();
}

function pageFullyChecked() {
  return state.items.length > 0 && (state.allResults || state.items.every((it) => state.checked.has(it.id)));
}

function renderBulk() {
  const allowed = bulkAllowed();
  const page = el("select-page");
  page.hidden = !allowed || state.items.length === 0;
  page.checked = pageFullyChecked();

  const count = state.allResults ? state.total : state.checked.size;
  const active = allowed && count > 0;
  el("bulkbar").hidden = !active;
  document.querySelector(".list-head").classList.toggle("bulk", active);
  el("bulk-count").textContent = state.allResults
    ? `All ${state.total} message(s) selected`
    : `${count} selected`;
  const all = el("bulk-all");
  all.hidden = state.allResults || !pageFullyChecked() || state.total <= state.items.length;
  all.textContent = `Select all ${state.total}`;
  const del = el("bulk-delete");
  del.textContent = inTrash() ? "Delete for good" : "Delete";
  del.classList.toggle("solid", inTrash());
  // In Trash the only sensible actions are restoring (Move) and deleting.
  for (const id of ["bulk-prio", "bulk-delay", "bulk-ignore", "bulk-read", "bulk-unread"]) {
    el(id).hidden = inTrash();
  }
}

function togglePage(on) {
  if (!on) {
    clearChecked();
  } else {
    for (const it of state.items) state.checked.add(it.id);
  }
  renderList();
}

function selectedCount() {
  return state.allResults ? state.total : state.checked.size;
}

// selectionBody says which messages a bulk action is for: the ticked ids, or
// every message the current box or search matches.
function selectionBody() {
  return state.allResults
    ? { all: true, in: state.query ? "" : state.box, q: state.query, unread: state.unreadOnly }
    : { ids: Array.from(state.checked) };
}

// runBulk sends one action for the whole selection. verb names it in the
// progress note ("Moving"), done in the result ("moved to Trash").
async function runBulk(action, verb, done) {
  const count = selectedCount();
  if (count === 0) return;
  const res = await busy(`${verb} ${count} message(s)…`,
    () => api("/api/messages/bulk", { method: "POST", body: { ...selectionBody(), ...action } }));
  clearChecked();
  const skipped = res.skipped ? `, ${res.skipped} skipped` : "";
  await afterBulk(`${res.moved || 0} message(s) ${done}${skipped}.`);
}

// busy shows that a slow request is running: a large move writes one file per
// message and can take a few seconds.
async function busy(note, run) {
  toast(note);
  document.body.classList.add("busy");
  try {
    return await run();
  } finally {
    document.body.classList.remove("busy");
  }
}

// confirmAll asks first when the selection is "all results": that reaches far
// beyond the page on screen. A ticked selection is visible, so it just runs.
function confirmAll(what, run) {
  if (!state.allResults) {
    run().catch(report);
    return;
  }
  const where = state.query ? `matching "${state.query}"` : `in ${boxName(state.box)}`;
  openModal(`${what} all ${state.total} message(s) ${where}?`, (body, done) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "primary";
    b.textContent = `${what} ${state.total} message(s)`;
    b.addEventListener("click", () => {
      done();
      run().catch(report);
    });
    body.append(b);
  });
}

function bulkMoveTo(box) {
  if (box === "trash") {
    confirmAll("Delete", () => runBulk({ action: "move", box }, "Moving", "moved to Trash"));
    return;
  }
  confirmAll(`Move to ${boxName(box)}:`, () => runBulk({ action: "move", box }, "Moving", `moved to ${boxName(box)}`));
}

function askBulkMove() {
  openModal(`Move ${selectedCount()} message(s) to which box?`, (body, done) => {
    const choices = document.createElement("div");
    choices.className = "choices";
    for (const box of state.boxes) {
      if (box.slug === state.box && !state.query) continue;
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = box.name;
      b.addEventListener("click", () => {
        done();
        bulkMoveTo(box.slug);
      });
      choices.append(b);
    }
    body.append(choices);
  });
}

function askBulkDelay() {
  askDays(`Delay ${selectedCount()} message(s) for how long?`, (days) => {
    confirmAll("Delay", () => runBulk({ action: "delay", days }, "Delaying", `delayed ${days} day(s)`));
  });
}

function askBulkIgnore() {
  const count = selectedCount();
  openModal(`Blacklist the senders of ${count} message(s)?`, (body, done) => {
    const p = document.createElement("p");
    p.textContent = "These messages move to Spam, and future mail from their senders goes straight to Spam.";
    body.append(p);
    const b = document.createElement("button");
    b.type = "button";
    b.className = "primary";
    b.textContent = "Ignore senders";
    b.addEventListener("click", () => {
      done();
      runBulk({ action: "ignore" }, "Ignoring", "moved to Spam, senders blacklisted").catch(report);
    });
    body.append(b);
  });
}

function bulkRead(read) {
  const action = read ? "read" : "unread";
  confirmAll(`Mark as ${action}:`, () => runBulk({ action }, "Marking", `marked ${action}`));
}

function confirmBulkDelete() {
  if (inTrash()) {
    confirmPurge();
    return;
  }
  bulkMoveTo("trash");
}

// afterBulk reloads after an action on many messages; the open message may be
// among them, so the reading pane starts empty.
async function afterBulk(note) {
  toast(note);
  showEmpty();
  await Promise.all([loadList(), refreshBoxes(), loadState()]);
}

function confirmDeleteSender() {
  const msg = state.message;
  if (!msg) return;
  const sender = msg.mail.from.address;
  if (!sender) {
    toast("This message has no sender address.", "error");
    return;
  }
  openModal(`Move all mail from ${sender} to Trash?`, (body, done) => {
    const p = document.createElement("p");
    p.textContent = "Every message from this sender, in every box. You can still get them back from Trash until you empty it.";
    body.append(p);
    const b = document.createElement("button");
    b.type = "button";
    b.className = "danger solid";
    b.textContent = "Move all to Trash";
    b.addEventListener("click", async () => {
      done();
      try {
        const res = await busy(`Moving mail from ${sender} to Trash…`,
          () => api(`/api/messages/${msg.id}/delete-sender`, { method: "POST", body: {} }));
        await afterBulk(`${res.moved || 0} message(s) from ${sender} moved to Trash.`);
      } catch (err) {
        report(err);
      }
    });
    body.append(b);
  });
}

// confirmPurge deletes the ticked Trash messages for good; ticking all of
// Trash is the same as emptying it.
function confirmPurge() {
  const count = state.allResults ? state.total : state.checked.size;
  if (count === 0) return;
  openModal(`Delete ${count} message(s) for good?`, (body, done) => {
    const p = document.createElement("p");
    p.textContent = "They are removed from this computer and cannot be brought back. " +
      "Nothing changes on your mail servers, and tidymail will not download them again.";
    body.append(p);
    const b = document.createElement("button");
    b.type = "button";
    b.className = "danger solid";
    b.textContent = `Delete ${count} for good`;
    b.addEventListener("click", async () => {
      done();
      try {
        const res = await busy(`Deleting ${count} message(s)…`, () => (state.allResults
          ? api("/api/boxes/trash/empty", { method: "POST", body: {} })
          : api("/api/messages/purge", { method: "POST", body: { ids: Array.from(state.checked) } })));
        clearChecked();
        await afterBulk(`${res.moved || 0} message(s) deleted for good.`);
      } catch (err) {
        report(err);
      }
    });
    body.append(b);
  });
}

function confirmEmptyTrash() {
  openModal(`Delete ${state.total} message(s) in Trash for good?`, (body, done) => {
    const p = document.createElement("p");
    p.textContent = "They are removed from this computer and cannot be brought back. " +
      "Nothing changes on your mail servers, and tidymail will not download them again.";
    body.append(p);
    const b = document.createElement("button");
    b.type = "button";
    b.className = "danger solid";
    b.textContent = "Empty trash";
    b.addEventListener("click", async () => {
      done();
      try {
        const res = await busy("Emptying Trash…", () => api("/api/boxes/trash/empty", { method: "POST", body: {} }));
        await afterBulk(`Trash emptied: ${res.moved || 0} message(s) deleted.`);
      } catch (err) {
        report(err);
      }
    });
    body.append(b);
  });
}

function openIndex(i) {
  if (i < 0 || i >= state.items.length) return;
  state.selected = i;
  renderList();
  document.querySelectorAll("#messages li")[i]?.scrollIntoView({ block: "nearest" });
  openMessage(state.items[i].id).catch(report);
}

/* ---------- reading pane ------------------------------------------------- */

async function openMessage(id) {
  const msg = await api(`/api/messages/${id}`);
  state.message = msg;
  renderMessage(msg);
  document.querySelector(".app").classList.add("reading");
  // The server marked it read; reflect that without a round trip.
  const row = state.items.find((it) => it.id === id);
  if (row && row.unread) {
    row.unread = false;
    renderList();
    refreshBoxes().catch(report);
  }
}

function renderMessage(msg) {
  el("empty-state").hidden = true;
  el("compose").hidden = true;
  el("newmail").hidden = true;
  el("message").hidden = false;

  el("msg-subject").textContent = msg.mail.subject || "(no subject)";
  el("msg-from").textContent = msg.mail.from.name
    ? `${msg.mail.from.name} <${msg.mail.from.address}>`
    : msg.mail.from.address || "(no sender)";
  el("msg-date").textContent = new Date(msg.mail.date).toLocaleString();

  const recipients = (msg.mail.to || []).concat(msg.mail.cc || []).map((a) => a.address);
  el("msg-to").textContent = recipients.length ? `to ${recipients.join(", ")}` : "";

  const flags = el("msg-flags");
  clear(flags);
  flags.append(span("tag", boxName(msg.box)));
  if (msg.whitelisted) flags.append(span("tag", "whitelisted"));
  if (msg.delayed_until) flags.append(span("tag", `delayed until ${humanDate(msg.delayed_until)}`));
  if (msg.prev_box) flags.append(span("tag", `was ${boxName(msg.prev_box)}`));
  for (const w of msg.mail.warnings || []) flags.append(span("tag", w));

  const atts = el("attachments");
  clear(atts);
  for (const att of msg.mail.attachments || []) {
    const li = document.createElement("li");
    const a = document.createElement("a");
    a.href = `/api/messages/${msg.id}/attachments/${att.part}`;
    a.setAttribute("download", att.filename);
    a.append(document.createTextNode(att.filename + " "));
    a.append(span("size", humanSize(att.size)));
    li.append(a);
    atts.append(li);
  }

  el("msg-raw").href = `/api/messages/${msg.id}/raw`;
  el("msg-body").textContent = msg.body || "(this message has no text body)";
  el("read-pane").scrollTop = 0;
}

/* ---------- actions ------------------------------------------------------ */

async function refreshBoxes() {
  const boxes = await api("/api/boxes");
  state.boxes = boxes;
  renderBoxes();
}

// afterAction drops the message from the current view and moves on, which is
// what makes working through a box feel like a queue.
async function afterAction(result, note) {
  if (result && Array.isArray(result.warnings)) {
    for (const w of result.warnings) toast(w, "warn");
  }
  if (note) toast(note);

  const wasSelected = state.selected;
  await Promise.all([loadList(true), refreshBoxes()]);
  if (state.items.length === 0) {
    showEmpty();
    return;
  }
  const next = Math.min(wasSelected, state.items.length - 1);
  openIndex(next < 0 ? 0 : next);
}

function showEmpty() {
  state.message = null;
  state.selected = -1;
  el("message").hidden = true;
  el("compose").hidden = true;
  // A new message being written stays open; it is only closed by Send or Cancel.
  const writing = !el("newmail").hidden;
  el("empty-state").hidden = writing;
  document.querySelector(".app").classList.toggle("reading", writing);
  renderList();
}

function currentID() {
  return state.message ? state.message.id : null;
}

async function actionPrio() {
  const id = currentID();
  if (!id) return;
  const res = await api(`/api/messages/${id}/prio`, { method: "POST", body: {} });
  await afterAction(res, "Moved to Priority.");
}

async function actionIgnore(applyExisting) {
  const id = currentID();
  if (!id) return;
  const res = await api(`/api/messages/${id}/ignore`, { method: "POST", body: { apply_existing: applyExisting } });
  const extra = res.moved > 1 ? ` (${res.moved} messages moved)` : "";
  await afterAction(res, `Blacklisted ${state.message.mail.from.address}${extra}.`);
}

async function actionDelay(days) {
  const id = currentID();
  if (!id) return;
  const res = await api(`/api/messages/${id}/delay`, { method: "POST", body: { days } });
  const until = res.message && res.message.delayed_until ? humanDate(res.message.delayed_until) : `${days}d`;
  await afterAction(res, `Delayed until ${until}, then back to ${boxName(res.message.prev_box)}.`);
}

async function actionMove(box) {
  const id = currentID();
  if (!id) return;
  const res = await api(`/api/messages/${id}/move`, { method: "POST", body: { box } });
  await afterAction(res, `Moved to ${boxName(box)}.`);
}

async function actionDelete() {
  const id = currentID();
  if (!id) return;
  const res = await api(`/api/messages/${id}/delete`, { method: "POST", body: {} });
  await afterAction(res, "Moved to Trash.");
}

function confirmArchiveAll() {
  openModal(`Archive all ${state.total} message(s) in Unprocessed?`, (body, done) => {
    const p = document.createElement("p");
    p.textContent = "They move to Processed here, and to the Archive folder on your IMAP account in the background.";
    body.append(p);
    const b = document.createElement("button");
    b.type = "button";
    b.className = "primary";
    b.textContent = "Archive all";
    b.addEventListener("click", () => {
      done();
      archiveAll().catch(report);
    });
    body.append(b);
  });
}

async function archiveAll() {
  await api("/api/boxes/unprocessed/move-all", { method: "POST", body: { box: "processed" } });
  toast("Archiving Unprocessed in the background…");
  showEmpty();

  // The server works through the box one message at a time; follow along.
  const started = Date.now();
  const follow = async () => {
    await Promise.all([loadList(true), refreshBoxes()]);
    const box = state.boxes.find((b) => b.slug === "unprocessed");
    if (!box || box.total === 0) {
      toast("Unprocessed is empty.");
      return;
    }
    if (Date.now() - started < 30 * 60 * 1000) {
      window.setTimeout(() => follow().catch(report), 3000);
    }
  };
  await follow();
}

async function sendReply(ev) {
  ev.preventDefault();
  const id = currentID();
  if (!id) return;
  const button = ev.submitter || el("compose").querySelector("button[type=submit]");
  const body = el("compose-body").value;
  if (!body.trim()) {
    toast("Write something first.", "error");
    return;
  }
  const cc = el("compose-cc").value.split(",").map((s) => s.trim()).filter(Boolean);
  if (filesTooBig("compose")) {
    toast("The attachments are larger than 20 MB together; remove some first.", "error");
    return;
  }

  button.disabled = true;
  document.body.classList.add("busy");
  try {
    const res = await sendWithFiles(`/api/messages/${id}/reply`, "compose",
      { body, cc, quote: el("compose-quote").checked });
    el("compose").hidden = true;
    el("compose-body").value = "";
    el("compose-cc").value = "";
    clearFiles("compose");
    await afterAction(res, "Reply sent, message filed as Processed.");
  } catch (err) {
    report(err);
  } finally {
    button.disabled = false;
    document.body.classList.remove("busy");
  }
}

function openCompose() {
  const msg = state.message;
  if (!msg) return;
  const to = (msg.reply_targets || []).map((a) => a.address).join(", ");
  if (!to) {
    toast("This message has no address to reply to.", "error");
    return;
  }
  const from = msg.reply_from || state.fromAddr;
  el("compose-to").textContent = `Re: ${to}${from ? `  (from ${from})` : ""}`;
  el("compose").hidden = false;
  el("compose-body").focus();
  el("compose").scrollIntoView({ block: "nearest" });
}

/* ---------- attachments --------------------------------------------------- */

// maxAttachBytes matches the server's limit for the files of one message.
const maxAttachBytes = 20 * 1024 * 1024;

function addFiles(form, list) {
  for (const f of list) state.files[form].push(f);
  renderFiles(form);
}

function clearFiles(form) {
  state.files[form] = [];
  el(`${form}-attach`).value = "";
  renderFiles(form);
}

function filesTooBig(form) {
  return state.files[form].reduce((n, f) => n + f.size, 0) > maxAttachBytes;
}

function renderFiles(form) {
  const list = el(`${form}-files`);
  clear(list);
  const tooBig = filesTooBig(form);
  state.files[form].forEach((f, i) => {
    const li = document.createElement("li");
    if (tooBig) li.classList.add("too-big");
    li.append(document.createTextNode(`${f.name} `), span("size", humanSize(f.size)));
    const drop = document.createElement("button");
    drop.type = "button";
    drop.title = `Remove ${f.name}`;
    drop.textContent = "×";
    drop.addEventListener("click", () => {
      state.files[form].splice(i, 1);
      renderFiles(form);
    });
    li.append(drop);
    list.append(li);
  });
}

// sendWithFiles posts JSON, or multipart with the JSON in a "json" field when
// files are attached.
function sendWithFiles(path, form, body) {
  const files = state.files[form];
  if (files.length === 0) return api(path, { method: "POST", body });
  const data = new FormData();
  data.append("json", JSON.stringify(body));
  for (const f of files) data.append("attachment", f, f.name);
  return api(path, { method: "POST", form: data });
}

function wireAttachments(form) {
  el(`${form}-attach`).addEventListener("change", (ev) => {
    addFiles(form, ev.target.files);
    ev.target.value = "";
  });
  const node = el(form);
  node.addEventListener("dragover", (ev) => {
    if (!ev.dataTransfer || !Array.from(ev.dataTransfer.types).includes("Files")) return;
    ev.preventDefault();
    node.classList.add("dropping");
  });
  node.addEventListener("dragleave", () => node.classList.remove("dropping"));
  node.addEventListener("drop", (ev) => {
    node.classList.remove("dropping");
    if (!ev.dataTransfer || ev.dataTransfer.files.length === 0) return;
    ev.preventDefault();
    addFiles(form, ev.dataTransfer.files);
  });
}

/* ---------- new message -------------------------------------------------- */

// openNewMail shows an empty message form in the reading pane, sending from
// the account of the open message when there is one.
function openNewMail() {
  const from = el("newmail-from");
  clear(from);
  for (const acct of state.accounts) {
    const opt = document.createElement("option");
    opt.value = acct.name;
    opt.textContent = acct.from_name ? `${acct.from_name} <${acct.from_addr}>` : acct.from_addr;
    from.append(opt);
  }
  const current = state.message && state.accounts.find((a) => a.from_addr === state.message.reply_from);
  if (current) from.value = current.name;

  el("empty-state").hidden = true;
  el("message").hidden = true;
  el("compose").hidden = true;
  el("newmail").hidden = false;
  document.querySelector(".app").classList.add("reading");
  el("newmail-to").focus();
}

function closeNewMail() {
  el("newmail").hidden = true;
  for (const id of ["newmail-to", "newmail-cc", "newmail-subject", "newmail-body"]) el(id).value = "";
  clearFiles("newmail");
  showEmpty();
}

function splitAddresses(text) {
  return text.split(/[,;]/).map((s) => s.trim()).filter(Boolean);
}

async function sendNewMail(ev) {
  ev.preventDefault();
  const to = splitAddresses(el("newmail-to").value);
  if (to.length === 0) {
    toast("Add at least one address in To.", "error");
    el("newmail-to").focus();
    return;
  }
  if (!el("newmail-body").value.trim()) {
    toast("Write something first.", "error");
    el("newmail-body").focus();
    return;
  }
  if (filesTooBig("newmail")) {
    toast("The attachments are larger than 20 MB together; remove some first.", "error");
    return;
  }
  const button = ev.submitter || el("newmail").querySelector("button[type=submit]");
  button.disabled = true;
  document.body.classList.add("busy");
  try {
    const res = await sendWithFiles("/api/send", "newmail", {
      account: el("newmail-from").value,
      to,
      cc: splitAddresses(el("newmail-cc").value),
      subject: el("newmail-subject").value,
      body: el("newmail-body").value,
    });
    for (const w of res.warnings || []) toast(w, "warn");
    toast(`Sent to ${to.join(", ")}.`);
    closeNewMail();
  } catch (err) {
    report(err);
  } finally {
    button.disabled = false;
    document.body.classList.remove("busy");
  }
}

/* ---------- modal -------------------------------------------------------- */

function openModal(title, build) {
  const modal = el("modal");
  el("modal-title").textContent = title;
  const body = el("modal-body");
  clear(body);
  build(body, closeModal);
  modal.hidden = false;
  const first = body.querySelector("button, input");
  if (first) first.focus();
}

function closeModal() {
  el("modal").hidden = true;
}

function askDelay() {
  if (!currentID()) return;
  askDays("Delay this message for how long?", (days) => actionDelay(days).catch(report));
}

// askDays offers a delay in days and calls pick with the choice.
function askDays(title, pick) {
  openModal(title, (body, done) => {
    const choices = document.createElement("div");
    choices.className = "choices";
    for (const days of [1, 3, 7, 14, 30, 90]) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = days === 1 ? "1 day" : `${days} days`;
      b.addEventListener("click", () => {
        done();
        pick(days);
      });
      choices.append(b);
    }
    body.append(choices);

    const custom = document.createElement("form");
    custom.className = "custom-delay";
    const input = document.createElement("input");
    input.type = "number";
    input.min = "1";
    input.max = "3650";
    input.value = "5";
    input.setAttribute("aria-label", "Days to delay");
    const go = document.createElement("button");
    go.type = "submit";
    go.className = "primary";
    go.textContent = "Delay";
    custom.append(input, go, span("", "days"));
    custom.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const days = Number.parseInt(input.value, 10);
      if (!Number.isFinite(days) || days < 1) {
        toast("Enter a whole number of days.", "error");
        return;
      }
      done();
      pick(days);
    });
    body.append(custom);
  });
}

function askMove() {
  if (!currentID()) return;
  openModal("Move to which box?", (body, done) => {
    const choices = document.createElement("div");
    choices.className = "choices";
    for (const box of state.boxes) {
      if (box.slug === state.message.box) continue;
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = box.name;
      b.addEventListener("click", () => {
        done();
        actionMove(box.slug).catch(report);
      });
      choices.append(b);
    }
    body.append(choices);

    const note = document.createElement("p");
    note.textContent = "Always send mail from this sender here?";
    body.append(note);

    const ruleRow = document.createElement("div");
    ruleRow.className = "choices";
    for (const box of state.boxes.filter((b) => !b.system || b.slug === "spam")) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = `Rule → ${box.name}`;
      b.addEventListener("click", () => {
        done();
        addRuleForSender(box.slug).catch(report);
      });
      ruleRow.append(b);
    }
    body.append(ruleRow);

    const subjNote = document.createElement("p");
    subjNote.textContent = "Or: always send mail whose subject contains…";
    body.append(subjNote);

    const subj = document.createElement("input");
    subj.type = "text";
    subj.placeholder = "invoice";
    subj.setAttribute("aria-label", "Word in the subject");
    body.append(subj);

    const subjRow = document.createElement("div");
    subjRow.className = "choices";
    for (const box of state.boxes.filter((b) => !b.system || b.slug === "spam")) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = `Rule → ${box.name}`;
      b.addEventListener("click", () => {
        const word = subj.value.trim();
        if (!word) {
          toast("Enter a word from the subject.", "error");
          return;
        }
        done();
        addRuleForSubject(word, box.slug).catch(report);
      });
      subjRow.append(b);
    }
    body.append(subjRow);
  });
}

function askIgnore() {
  const msg = state.message;
  if (!msg) return;
  openModal(`Blacklist ${msg.mail.from.address}?`, (body, done) => {
    const p = document.createElement("p");
    p.textContent = "Future mail from this sender goes straight to Spam.";
    body.append(p);
    const choices = document.createElement("div");
    choices.className = "choices";

    const one = document.createElement("button");
    one.type = "button";
    one.className = "primary";
    one.textContent = "This message only";
    one.addEventListener("click", () => {
      done();
      actionIgnore(false).catch(report);
    });

    const all = document.createElement("button");
    all.type = "button";
    all.textContent = "This and every earlier one";
    all.addEventListener("click", () => {
      done();
      actionIgnore(true).catch(report);
    });

    choices.append(one, all);
    body.append(choices);
  });
}

async function addRuleForSender(box) {
  const msg = state.message;
  if (!msg) return;
  const res = await api("/api/rules", {
    method: "POST",
    body: { match: "from", address: msg.mail.from.address, box, apply_existing: true },
  });
  await afterAction(res, `Mail from ${msg.mail.from.address} now goes to ${boxName(box)} (${res.moved || 0} moved).`);
}

async function addRuleForSubject(word, box) {
  const res = await api("/api/rules", {
    method: "POST",
    body: { match: "subject", address: word, box, apply_existing: true },
  });
  await afterAction(res, `Mail with "${word}" in the subject now goes to ${boxName(box)} (${res.moved || 0} moved).`);
}

function confirmDeleteBox(box) {
  openModal(`Delete the ${box.name} box?`, (body, done) => {
    const p = document.createElement("p");
    p.textContent = `Its ${box.total} message(s) move to Unprocessed, and rules pointing at it are removed. Nothing is deleted from disk.`;
    body.append(p);
    const b = document.createElement("button");
    b.type = "button";
    b.className = "primary";
    b.textContent = "Delete box";
    b.addEventListener("click", async () => {
      done();
      try {
        await api(`/api/boxes/${box.slug}`, { method: "DELETE" });
        if (state.box === box.slug) state.box = "unprocessed";
        await Promise.all([refreshBoxes(), loadList()]);
        toast(`Box ${box.name} removed.`);
      } catch (err) {
        report(err);
      }
    });
    body.append(b);
  });
}

/* ---------- refresh------------------------------------------------------ */

// refresh re-reads mail.d. Fetching is 'tidymail sync', a separate process;
// this only picks up what it stored.
async function refresh() {
  await Promise.all([loadState(), loadList(true)]);
  toast("Refreshed.");
}

/* ---------- keyboard ----------------------------------------------------- */

function isTyping(target) {
  const tag = target.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.isContentEditable;
}

function onKey(ev) {
  if (ev.key === "Escape") {
    if (!el("modal").hidden) {
      closeModal();
      return;
    }
    if (!el("compose").hidden) {
      el("compose").hidden = true;
      return;
    }
    if (!el("newmail").hidden && !isTyping(ev.target)) {
      closeNewMail();
      return;
    }
    if (isTyping(ev.target)) ev.target.blur();
    return;
  }
  if (isTyping(ev.target) || ev.ctrlKey || ev.metaKey || ev.altKey) return;

  switch (ev.key) {
    case "j":
      ev.preventDefault();
      openIndex(state.selected + 1);
      break;
    case "k":
      ev.preventDefault();
      openIndex(state.selected - 1);
      break;
    case "r":
      ev.preventDefault();
      openCompose();
      break;
    case "i":
      ev.preventDefault();
      askIgnore();
      break;
    case "p":
      ev.preventDefault();
      actionPrio().catch(report);
      break;
    case "d":
      ev.preventDefault();
      askDelay();
      break;
    case "m":
      ev.preventDefault();
      askMove();
      break;
    case "x":
      ev.preventDefault();
      if (bulkAllowed() && state.selected >= 0 && state.selected < state.items.length) {
        const id = state.items[state.selected].id;
        toggleChecked(id, !(state.allResults || state.checked.has(id)));
      }
      break;
    case "#":
    case "Delete":
      ev.preventDefault();
      actionDelete().catch(report);
      break;
    case "g":
      ev.preventDefault();
      refresh().catch(report);
      break;
    case "c":
      ev.preventDefault();
      openNewMail();
      break;
    case "/":
      ev.preventDefault();
      el("search").focus();
      break;
    default:
      break;
  }
}

/* ---------- wiring ------------------------------------------------------- */

function debounce(fn, ms) {
  let timer = 0;
  return (...args) => {
    window.clearTimeout(timer);
    timer = window.setTimeout(() => fn(...args), ms);
  };
}

function wire() {
  el("refresh").addEventListener("click", () => refresh().catch(report));
  el("new-mail").addEventListener("click", openNewMail);
  el("newmail").addEventListener("submit", (ev) => { sendNewMail(ev).catch(report); });
  el("newmail-cancel").addEventListener("click", closeNewMail);
  wireAttachments("newmail");
  wireAttachments("compose");
  el("archive-all").addEventListener("click", confirmArchiveAll);
  el("empty-trash").addEventListener("click", confirmEmptyTrash);
  el("select-page").addEventListener("change", (ev) => togglePage(ev.target.checked));
  el("bulk-all").addEventListener("click", () => {
    state.allResults = true;
    renderList();
  });
  el("bulk-move").addEventListener("click", askBulkMove);
  el("bulk-prio").addEventListener("click", () => bulkMoveTo("priority"));
  el("bulk-delay").addEventListener("click", askBulkDelay);
  el("bulk-ignore").addEventListener("click", askBulkIgnore);
  el("bulk-read").addEventListener("click", () => bulkRead(true));
  el("bulk-unread").addEventListener("click", () => bulkRead(false));
  el("bulk-delete").addEventListener("click", confirmBulkDelete);
  el("bulk-clear").addEventListener("click", () => {
    clearChecked();
    renderList();
  });

  el("search").addEventListener("input", debounce((ev) => {
    state.query = ev.target.value.trim();
    state.offset = 0;
    clearChecked();
    loadList().catch(report);
  }, 200));

  el("unread-only").addEventListener("change", (ev) => {
    state.unreadOnly = ev.target.checked;
    state.offset = 0;
    clearChecked();
    loadList().catch(report);
  });

  el("prev-page").addEventListener("click", () => {
    state.offset = Math.max(0, state.offset - state.limit);
    loadList().catch(report);
  });
  el("next-page").addEventListener("click", () => {
    state.offset += state.limit;
    loadList().catch(report);
  });

  el("newbox-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const input = el("newbox-name");
    const name = input.value.trim();
    if (!name) return;
    try {
      const box = await api("/api/boxes", { method: "POST", body: { name } });
      input.value = "";
      await refreshBoxes();
      toast(`Box ${box.name} created.`);
    } catch (err) {
      report(err);
    }
  });

  for (const button of document.querySelectorAll(".actions button[data-act]")) {
    button.addEventListener("click", () => {
      switch (button.dataset.act) {
        case "reply": openCompose(); break;
        case "ignore": askIgnore(); break;
        case "prio": actionPrio().catch(report); break;
        case "delay": askDelay(); break;
        case "move": askMove(); break;
        case "delete": actionDelete().catch(report); break;
        case "delete-sender": confirmDeleteSender(); break;
        default: break;
      }
    });
  }

  el("compose").addEventListener("submit", (ev) => { sendReply(ev).catch(report); });
  el("compose-cancel").addEventListener("click", () => { el("compose").hidden = true; });
  el("modal-cancel").addEventListener("click", closeModal);
  el("modal").addEventListener("click", (ev) => {
    if (ev.target === el("modal")) closeModal();
  });
  document.addEventListener("keydown", onKey);
}

async function main() {
  wire();
  try {
    await loadState();
    await loadList();
  } catch (err) {
    report(err);
  }
}

main().catch(report);

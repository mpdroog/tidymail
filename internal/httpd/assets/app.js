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
  box: "unprocessed",
  query: "",
  unreadOnly: false,
  offset: 0,
  limit: 50,
  total: 0,
  items: [],
  selected: -1,
  message: null,
};

const el = (id) => document.getElementById(id);

/* ---------- server calls ------------------------------------------------- */

async function api(path, options = {}) {
  const opts = { credentials: "same-origin", headers: {}, ...options };
  if (opts.body !== undefined) {
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

  state.items.forEach((item, i) => {
    const li = document.createElement("li");
    li.tabIndex = 0;
    li.setAttribute("aria-selected", String(i === state.selected));
    if (item.unread) li.classList.add("unread");

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
  el("empty-state").hidden = false;
  document.querySelector(".app").classList.remove("reading");
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

  button.disabled = true;
  document.body.classList.add("busy");
  try {
    const res = await api(`/api/messages/${id}/reply`, {
      method: "POST",
      body: { body, cc, quote: el("compose-quote").checked },
    });
    el("compose").hidden = true;
    el("compose-body").value = "";
    el("compose-cc").value = "";
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
  el("compose-to").textContent = `Re: ${to}${state.fromAddr ? `  (from ${state.fromAddr})` : ""}`;
  el("compose").hidden = false;
  el("compose-body").focus();
  el("compose").scrollIntoView({ block: "nearest" });
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
  openModal("Delay this message for how long?", (body, done) => {
    const choices = document.createElement("div");
    choices.className = "choices";
    for (const days of [1, 3, 7, 14, 30, 90]) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = days === 1 ? "1 day" : `${days} days`;
      b.addEventListener("click", () => {
        done();
        actionDelay(days).catch(report);
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
      actionDelay(days).catch(report);
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
    case "#":
    case "Delete":
      ev.preventDefault();
      actionDelete().catch(report);
      break;
    case "g":
      ev.preventDefault();
      refresh().catch(report);
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
  el("archive-all").addEventListener("click", confirmArchiveAll);

  el("search").addEventListener("input", debounce((ev) => {
    state.query = ev.target.value.trim();
    state.offset = 0;
    loadList().catch(report);
  }, 200));

  el("unread-only").addEventListener("change", (ev) => {
    state.unreadOnly = ev.target.checked;
    state.offset = 0;
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

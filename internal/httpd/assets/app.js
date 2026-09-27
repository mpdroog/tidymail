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
  summaries: false, // whether the server can make AI summaries
  box: "unprocessed",
  query: "",
  jobs: new Map(), // background moves by id, as last reported
  seenTotal: -1, // messages across all boxes when the list was last loaded
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
  state.summaries = data.summaries;
  renderBoxes();
  renderStats(data.stats);
}

// unreadIn is a box's unread count as the UI shows it: mail in Trash is never
// news, so Trash has none.
function unreadIn(box) {
  return box.slug === "trash" ? 0 : box.unread;
}

// updateListActions shows the whole-box buttons that apply to the box in view.
function updateListActions() {
  el("archive-all").hidden = !(state.box === "unprocessed" && !state.query && state.total > 0);
  const box = state.boxes.find((b) => b.slug === state.box);
  el("mark-seen").hidden = !(box && !state.query && unreadIn(box) > 0);
  el("filter-body").hidden = !(!state.query && state.box !== "trash" && state.total > 0);
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
    const unread = unreadIn(box);
    const count = span(unread > 0 ? "count unread" : "count", unread > 0 ? `${unread}/${box.total}` : String(box.total));
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
  updateListActions();
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
    if (item.unread && item.box !== "trash") li.classList.add("unread");

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
  updateListActions();
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

  renderSummary(msg);
  el("msg-raw").href = `/api/messages/${msg.id}/raw`;
  el("msg-body").textContent = msg.body || "(this message has no text body)";
  el("read-pane").scrollTop = 0;
}

/* ---------- summaries ---------------------------------------------------- */

// renderSummary shows the stored summary of msg, or the button to make one.
function renderSummary(msg) {
  const sum = msg.summary;
  el("summary").hidden = !sum;
  el("summarize").hidden = !!sum || !state.summaries;
  el("summarize").disabled = false;
  el("summarize").textContent = "Summarize ";
  el("summarize").append(kbd("s"));
  if (!sum) return;
  el("summary-tldr").textContent = sum.tldr;
  el("summary-details").textContent = sum.details;
  el("summary-details").hidden = true;
  el("summary-more").hidden = !sum.details;
  el("summary-more").textContent = "Explain more";
}

function kbd(text) {
  const k = document.createElement("kbd");
  k.textContent = text;
  return k;
}

function toggleSummaryDetails() {
  const details = el("summary-details");
  details.hidden = !details.hidden;
  el("summary-more").textContent = details.hidden ? "Explain more" : "Less";
}

// summarize asks for a summary of the open message. It takes a few seconds;
// if the reader has moved on by then, it is kept for when they come back.
async function summarize() {
  const msg = state.message;
  if (!msg || !state.summaries) return;
  if (msg.summary) {
    toggleSummaryDetails();
    return;
  }
  const button = el("summarize");
  if (button.disabled) return;
  button.disabled = true;
  button.textContent = "Summarizing…";
  try {
    const sum = await api(`/api/messages/${msg.id}/summary`, { method: "POST", body: {} });
    msg.summary = sum;
    if (state.message && state.message.id === msg.id) renderSummary(msg);
  } catch (err) {
    if (state.message && state.message.id === msg.id) renderSummary(msg);
    report(err);
  }
}

/* ---------- actions ------------------------------------------------------ */

async function refreshBoxes() {
  const boxes = await api("/api/boxes");
  state.boxes = boxes;
  renderBoxes();
}

/* ---------- new mail ----------------------------------------------------- */

// /api/received only grows when 'tidymail sync' stores mail outside Trash;
// moves never change it. A higher count than when the page last loaded means
// it is out of date. Taken before the list loads, so nothing slips between.
async function markSeen() {
  const received = await api("/api/received");
  state.seenTotal = received.count;
  el("new-mail").hidden = true;
}

async function checkNewMail() {
  if (document.hidden || state.seenTotal < 0) return;
  const received = await api("/api/received");
  const arrived = received.count - state.seenTotal;
  const banner = el("new-mail");
  if (arrived > 0) {
    banner.textContent = `${arrived} new message(s) arrived. Refresh (g)`;
    banner.hidden = false;
  }
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

// moveOnlyThis moves the open message and, when address is set, adds a rule
// so later mail from that sender lands in the same box.
async function moveOnlyThis(box, address) {
  if (address) {
    await api("/api/rules", { method: "POST", body: { match: "from", address, box, apply_existing: false } });
    toast(`Later mail from ${address} goes to ${boxName(box)} too.`);
  }
  await actionMove(box);
}

async function actionRemove() {
  const id = currentID();
  if (!id) return;
  const res = await api(`/api/messages/${id}/delete`, { method: "POST", body: {} });
  await afterAction(res, "Moved to Trash.");
}

// senderList renders the preview of a sender's mail: how many, and the newest.
function senderList(preview, body) {
  const p = document.createElement("p");
  p.textContent = preview.total > preview.items.length
    ? `${preview.total} message(s) from this sender in ${boxName(preview.box)}, the newest ${preview.items.length} below.`
    : `${preview.total} message(s) from this sender in ${boxName(preview.box)}:`;
  body.append(p);

  const list = document.createElement("ul");
  list.className = "sender-list";
  for (const item of preview.items) {
    const li = document.createElement("li");
    li.append(span("sender-date", humanDate(item.date)));
    li.append(span("sender-subject", item.subject || "(no subject)"));
    list.append(li);
  }
  body.append(list);
}

// askRemove trashes the open message. When the sender has more mail in the
// same box it offers to take all of it, showing what that would be first.
async function askRemove() {
  const id = currentID();
  if (!id) return;
  if (state.message.box === "trash") return;
  const preview = await api(`/api/messages/${id}/sender`);
  if (preview.total <= 1) {
    await actionRemove();
    return;
  }
  openModal(`Remove mail from ${preview.address}?`, (body, done) => {
    senderList(preview, body);
    const note = document.createElement("p");
    note.textContent = "They also move to Trash on your IMAP account. Later mail from this sender still arrives.";
    body.append(note);

    const choices = document.createElement("div");
    choices.className = "choices";
    const all = document.createElement("button");
    all.type = "button";
    all.className = "danger";
    all.textContent = `Move ${preview.total} to Trash`;
    all.addEventListener("click", () => {
      done();
      startSenderMove("trash", false).catch(report);
    });
    const one = document.createElement("button");
    one.type = "button";
    one.textContent = "Only this email";
    one.addEventListener("click", () => {
      done();
      actionRemove().catch(report);
    });
    choices.append(all, one);
    body.append(choices);
  });
}

// startSenderMove moves everything from the open message's sender to box in
// the background; the jobs panel shows how far it is.
async function startSenderMove(box, rule) {
  const id = currentID();
  if (!id) return;
  const job = await api(`/api/messages/${id}/move-sender`, { method: "POST", body: { box, rule } });
  if (rule) toast(`Later mail from ${job.address} goes to ${boxName(box)} too.`);
  state.jobs.set(job.id, job);
  renderJobs();
  pollJobs();
}

async function markBoxSeen(box) {
  const job = await api(`/api/boxes/${box}/mark-seen`, { method: "POST", body: {} });
  el("mark-seen").hidden = true;
  state.jobs.set(job.id, job);
  renderJobs();
  pollJobs();
}

/* ---------- background jobs ---------------------------------------------- */

function jobLabel(job) {
  switch (job.kind) {
    case "seen": return `${boxName(job.box)}: marking all seen`;
    case "filter": return `${boxName(job.box)}: “${job.pattern}” → Trash`;
    default: return `${job.address} → ${boxName(job.box)}`;
  }
}

function jobDone(job) {
  switch (job.kind) {
    case "seen": return `Marked ${job.moved} message(s) in ${boxName(job.box)} as seen.`;
    case "filter": return `Moved ${job.moved} message(s) matching “${job.pattern}” to Trash.`;
    default: return `Moved ${job.moved} message(s) from ${job.address} to ${boxName(job.box)}.`;
  }
}

function renderJobs() {
  const panel = el("jobs");
  clear(panel);
  for (const job of state.jobs.values()) {
    if (job.finished) continue;
    const row = document.createElement("div");
    row.className = "job";
    const head = document.createElement("div");
    head.className = "job-head";
    head.append(span("job-label", jobLabel(job)));
    head.append(span("job-count", `${job.done} / ${job.total}`));
    row.append(head);
    const bar = document.createElement("progress");
    bar.max = Math.max(job.total, 1);
    bar.value = job.done;
    row.append(bar);
    panel.append(row);
  }
  panel.hidden = panel.childElementCount === 0;
}

// pollJobs follows running jobs until they are all done, then reloads the
// view once per finished job. It also picks up jobs started before a reload.
let jobTimer = 0;
async function pollJobs() {
  window.clearTimeout(jobTimer);
  const list = await api("/api/jobs");
  let movedNow = false;
  let seenNow = false;
  for (const job of list) {
    const before = state.jobs.get(job.id);
    if (job.finished && before && !before.finished) {
      if (job.kind === "seen") seenNow = true;
      else movedNow = true;
      if (job.error) toast(`Stopped after ${job.moved} message(s): ${job.error}`, "error");
      else toast(jobDone(job));
    }
    if (before || !job.finished) state.jobs.set(job.id, job);
  }
  renderJobs();
  // A move took the open message away, so go on to the next like any action;
  // marking seen changes nothing in view but the styling.
  if (movedNow) await afterAction(null, "");
  else if (seenNow) await Promise.all([loadList(true), refreshBoxes()]);
  if ([...state.jobs.values()].some((j) => !j.finished)) {
    jobTimer = window.setTimeout(() => { pollJobs().catch(report); }, 500);
  }
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

async function askMove() {
  const id = currentID();
  if (!id) return;
  const current = state.message.box;
  const preview = await api(`/api/messages/${id}/sender`);
  openModal(`Move mail from ${preview.address}`, (body, done) => {
    senderList(preview, body);

    const checkbox = (label) => {
      const wrap = document.createElement("label");
      wrap.className = "check";
      const input = document.createElement("input");
      input.type = "checkbox";
      wrap.append(input, document.createTextNode(` ${label}`));
      body.append(wrap);
      return input;
    };
    const only = checkbox("Only this email");
    const rule = checkbox(`Always move email from ${preview.address} to the chosen folder`);
    rule.checked = true;

    const choices = document.createElement("div");
    choices.className = "choices";
    for (const box of state.boxes) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = box.name;
      // Moving to the box the mail is already in would do nothing.
      b.disabled = box.slug === current;
      b.addEventListener("click", () => {
        done();
        if (only.checked) moveOnlyThis(box.slug, rule.checked ? preview.address : "").catch(report);
        else startSenderMove(box.slug, rule.checked).catch(report);
      });
      choices.append(b);
    }
    body.append(choices);
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

/* ---------- body filter -------------------------------------------------- */

// filterDelay is how long typing must pause before the preview is fetched.
const filterDelay = 600;
const filterMinChars = 3;

// askBodyFilter trashes every message in box whose text contains a pattern.
// The preview follows the pattern as it is typed, so what the button does is
// always on screen before it is pressed.
function askBodyFilter(box) {
  openModal(`Remove by text in ${boxName(box)}`, (body, done) => {
    const hint = document.createElement("p");
    hint.textContent = "Trashes every message here whose text contains this, ignoring case. % matches anything in between, e.g. backup % failed.";
    body.append(hint);

    const input = document.createElement("input");
    input.type = "text";
    input.className = "filter-input";
    input.placeholder = "%text in the body%";
    input.autocomplete = "off";
    body.append(input);

    const summary = document.createElement("p");
    summary.className = "filter-summary";
    const list = document.createElement("ul");
    list.className = "sender-list match-list";
    list.hidden = true;
    const go = document.createElement("button");
    go.type = "button";
    go.className = "danger";
    go.disabled = true;
    go.textContent = "Move to Trash";
    body.append(summary, list, go);

    let pattern = "";
    let asked = 0; // only the answer to the latest request is shown
    const show = async () => {
      const typed = input.value.trim();
      const chars = typed.replace(/[%\s]/g, "").length;
      go.disabled = true;
      clear(list);
      list.hidden = true;
      if (chars < filterMinChars) {
        summary.textContent = typed ? `Type at least ${filterMinChars} characters.` : "";
        return;
      }
      const mine = ++asked;
      summary.textContent = "Searching…";
      let res;
      try {
        res = await api(`/api/boxes/${box}/body-matches?${new URLSearchParams({ body: typed })}`);
      } catch (err) {
        if (mine === asked) summary.textContent = err.message;
        return;
      }
      if (mine !== asked) return;
      pattern = typed;
      summary.textContent = res.total > res.items.length
        ? `${res.total} message(s) match, the newest ${res.items.length} below.`
        : `${res.total} message(s) match.`;
      for (const item of res.items) {
        const li = document.createElement("li");
        li.append(span("sender-date", humanDate(item.date)));
        li.append(span("sender-subject", `${displayName(item)}: ${item.subject || "(no subject)"}`));
        li.append(span("match-snippet", item.snippet));
        list.append(li);
      }
      list.hidden = res.total === 0;
      go.textContent = `Move ${res.total} to Trash`;
      go.disabled = res.total === 0;
    };
    const later = debounce(() => { show().catch(report); }, filterDelay);
    input.addEventListener("input", () => {
      ++asked; // a pending answer is for an older pattern now
      go.disabled = true;
      later();
    });

    go.addEventListener("click", () => {
      done();
      trashBodyMatches(box, pattern).catch(report);
    });
  });
}

async function trashBodyMatches(box, pattern) {
  const job = await api(`/api/boxes/${box}/trash-body-matches`, { method: "POST", body: { body: pattern } });
  state.jobs.set(job.id, job);
  renderJobs();
  pollJobs();
}

/* ---------- rules -------------------------------------------------------- */

// showRules lists every routing rule with a Remove button each. Removing a
// rule only changes where later mail lands; nothing already filed moves.
async function showRules() {
  const rules = await api("/api/rules");
  rules.sort((a, b) => boxName(a.box).localeCompare(boxName(b.box)) || a.address.localeCompare(b.address));
  openModal(`Rules (${rules.length})`, (body) => {
    const p = document.createElement("p");
    p.textContent = rules.length === 0
      ? "No rules yet. Move (m) with \"Always move email from …\" ticked creates one, and so does Ignore (i)."
      : "Incoming mail from these senders goes straight to their box. Removing a rule leaves mail already filed where it is.";
    body.append(p);
    if (rules.length === 0) return;

    const list = document.createElement("ul");
    list.className = "sender-list rule-list";
    for (const rule of rules) {
      const li = document.createElement("li");
      const who = rule.match === "from" ? rule.address : `${rule.match}: ${rule.address}`;
      li.append(span("sender-subject", who));
      li.append(span("tag", boxName(rule.box)));
      const b = document.createElement("button");
      b.type = "button";
      b.className = "ghost small";
      b.textContent = "Remove";
      b.addEventListener("click", async () => {
        b.disabled = true;
        try {
          await api("/api/rules/delete", { method: "POST", body: { match: rule.match, address: rule.address } });
          toast(`Rule removed: ${rule.address} no longer goes to ${boxName(rule.box)}.`);
          await loadState();
          await showRules();
        } catch (err) {
          b.disabled = false;
          report(err);
        }
      });
      li.append(b);
      list.append(li);
    }
    body.append(list);
  });
}

/* ---------- refresh------------------------------------------------------ */

// refresh re-reads mail.d. Fetching is 'tidymail sync', a separate process;
// this only picks up what it stored.
async function refresh() {
  await markSeen();
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
      askMove().catch(report);
      break;
    case "x":
    case "Delete":
      ev.preventDefault();
      askRemove().catch(report);
      break;
    case "g":
      ev.preventDefault();
      refresh().catch(report);
      break;
    case "s":
      ev.preventDefault();
      summarize().catch(report);
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
  el("filter-body").addEventListener("click", () => askBodyFilter(state.box));
  el("summary-more").addEventListener("click", toggleSummaryDetails);
  el("show-rules").addEventListener("click", () => { showRules().catch(report); });
  el("mark-seen").addEventListener("click", () => { markBoxSeen(state.box).catch(report); });

  el("search").addEventListener("input", debounce((ev) => {
    state.query = ev.target.value.trim();
    state.offset = 0;
    loadList().catch(report);
  }, 200));

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
        case "move": askMove().catch(report); break;
        case "remove": askRemove().catch(report); break;
        case "summarize": summarize().catch(report); break;
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

  el("new-mail").addEventListener("click", () => { refresh().catch(report); });
  window.setInterval(() => { checkNewMail().catch(report); }, 30 * 1000);
  document.addEventListener("visibilitychange", () => { checkNewMail().catch(report); });
}

async function main() {
  wire();
  try {
    await loadState();
    await markSeen();
    await loadList();
    await pollJobs();
  } catch (err) {
    report(err);
  }
}

main().catch(report);

# tidymail

A local IMAP mail client with a browser UI, built around processing mail as a
queue rather than filing it in folders by hand.

Mail is fetched into a `mail.d/` directory of plain text files. Every message
gets one box, and every message in a box has four ways out: **reply**,
**ignore**, **prio**, **delay**. That is the whole flow.

- Written in Go, no cgo (`modernc.org/sqlite`), one static binary.
- Everything is in `mail.d/`, in text: commit it to git and sync it between
  machines. The SQLite file next to it is a rebuildable search index, not
  state.
- The UI runs under a strict Content-Security-Policy with no inline script or
  style. HTML mail is flattened to text before it is ever stored, so a hostile
  message has nothing to execute.

## Quick start

```sh
make build
./tidymail init                       # writes mail.d/config.toml
$EDITOR mail.d/config.toml            # fill in your account
export TIDYMAIL_IMAP_PASSWORD=...     # if you used env: in the config
export TIDYMAIL_SMTP_PASSWORD=...
./tidymail sync                       # fetch mail, then exit
./tidymail serve                      # http://127.0.0.1:8080
```

Other commands:

```sh
./tidymail reindex   # rebuild mail.d/index.db from the text files
./tidymail version
```

`-dir` points at a different `mail.d` if you keep more than one. `sync` logs
one statistics line per batch; add `-v` to also log every stored message.

## Fetching mail

Only `tidymail sync` reads your mailbox; `serve` never does, so there is never
more than one process fetching. Run `sync` by hand or on a timer, for example
from cron (cron has no shell environment, so use `file:` secrets):

```
*/5 * * * * cd /path/to/tidymail && ./tidymail sync 2>&1 | logger -t tidymail
```

A running `serve` shows what `sync` stored when you press `g` (Refresh) or
reload the page; an open page checks every 30 seconds and shows a banner when
new mail has arrived. `serve` is also what returns delayed mail to its box, at
startup and every minute while it runs.

Downloading is usually what limits a sync, so `sync` fetches batches over
`imap.connections` connections at once (default 3, at most 10; Gmail allows 15
per account). Batches are still stored in order, so interrupting a sync never
skips mail.

When `sync` stores new mail outside Spam it shows one desktop notification over
D-Bus (`notify = false` turns that off). From cron, where
`DBUS_SESSION_BUS_ADDRESS` is not set, it uses `/run/user/<uid>/bus`.

## The flow

Incoming mail is routed on arrival, first match wins:

1. a **rule** matching the sender or a recipient sends it to that rule's box
2. a sender in `contacts.txt` (someone you have written to) → **Whitelisted**
3. everything else → **Unprocessed**

`contacts.txt` fills itself: tidymail scans your IMAP Sent folder on every sync
and records everyone you have written to, and adds anyone you reply to from
inside the app.

From any box, on any message:

| Key | Action  | Result |
|-----|---------|--------|
| `r` | Reply   | sends over SMTP, appends to your IMAP Sent folder, moves the message to **Processed** |
| `i` | Ignore  | blacklists the sender (a rule to Spam) and moves the message to **Spam**; optionally sweeps up everything you already have from them |
| `p` | Prio    | moves to **Priority** |
| `d` | Delay   | asks for a number of days and parks it in **Delayed**; when the time is up it returns to *the box it was in*, not to the inbox |
| `m` | Move    | the sender's mail in this box (or, ticked, only this message) to any box; "Always move email from …", on by default, adds a rule so later mail lands there too |
| `x` | Remove  | moves to **Trash**, and to the Trash folder on your IMAP account; when the sender has more mail in this box, it lists it (the newest 50) and offers to trash all of it |

Other keys: `j`/`k` next/previous, `/` search, `g` refresh, `Esc` back out.

**Summarize** (`s`) asks Claude for a TL;DR of the open message (at most four
sentences), with a paragraph more behind **Explain more** (or `s` again). It
runs the Claude Code CLI (`claude -p`), so it uses that login, a Claude
subscription included, and needs no API key; the CLI is found on `PATH` or in
`~/.local/bin`, or set `[summary] command = "/path/to/claude"` (and optionally
`model`) in `config.toml`. The CLI runs with all tools, MCP servers and settings
off, in an empty directory, so a message can only ever influence its own
summary. Only the message you summarize leaves the machine. Summaries are kept
in `mail.d/summaries/`, so each message is summarized once.

Custom boxes are added in the sidebar. "Error mails" is the obvious one: make
the box, then **Move → Error mails** on a message from your CI: what is already
in the box moves along, and every later mail from that sender files itself.

Boxes `unprocessed`, `whitelisted`, `priority`, `delayed`, `processed`,
`spam` and `trash` are built in and cannot be deleted.

Move and Remove only take mail from the box the message is in; the sender's
mail in other boxes stays where it is. Future mail from the sender is only
routed by a rule: `i` (Ignore) creates one for Spam, and Move creates one unless
you untick "Always move email from …".

**Filter**, at the top of any box but Trash, trashes every message in that box
whose text contains a pattern (ignoring case; `%` matches anything in between,
as in `backup % failed`). It previews the count and the newest 50 matches, with
the matching text, as you type.

**Archive all**, at the top of Unprocessed, moves everything there to Processed
at once. Deleting a custom box moves its mail
to Unprocessed and drops rules pointing at it. Nothing is ever removed from
disk.

## Search

The search box queries a SQLite FTS5 index over subject, body and addresses:

```
invoice                     both words anywhere
from:ann@example.com        restrict to addresses
subject:renewal             restrict to subjects
quarter*                    prefix match
```

Searching looks across every box, so a message you delayed six weeks ago is
still one query away.

## What lives in mail.d

```
mail.d/
  config.toml            your account settings
  boxes.toml             box definitions
  rules.toml             address -> box routing
  contacts.txt           the whitelist, one address per line
  sync.json              IMAP cursor (UIDVALIDITY, last UID seen)
  mail/2026/08/<id>.eml  the raw message, never modified
  mail/2026/08/<id>.txt  its extracted plain text
  state/ab/<id>.json     which box it is in, and until when
  outbox/<ts>-<id>.eml   replies you sent
  index.db               derived search index (gitignored)
```

`<id>` is the SHA-256 of the raw message, so re-fetching the same mail is a
no-op and the archive can never hold a duplicate.

The split matters for syncing: message files are immutable, so git never has a
conflict in them. Only the small `state/*.json` files change, and only one line
at a time. `index.db` is in `.gitignore` because it is derived — on a second
machine tidymail notices an empty index and rebuilds it from the text files.

**Secrets:** if you are going to commit `mail.d`, do not put passwords in
`config.toml`. Use `password = "env:TIDYMAIL_IMAP_PASSWORD"` or
`password = "file:secrets/imap.txt"` with that file ignored.

## What tidymail does to your IMAP account

The local store is the system of record: tidymail decides which box a message
is in, and `serve` repeats that in IMAP folders in the background, so your phone
sees the same sorting:

| Box | IMAP folder |
|-----|-------------|
| Unprocessed | INBOX (the Archive folder with `after_fetch = "archive"` or `"delete"`) |
| Processed | the Archive folder (`\Archive`, Gmail's All Mail, or `Archive`) |
| Spam | `imap.junk`, the `\Junk` folder, or `Spam` |
| Trash | `imap.trash`, the `\Trash` folder, or `Trash` |
| any other box | a folder named after the box, created when needed |

Messages are found on the server by Message-ID. Every move counts: `m`, Ignore,
Delete, rules applied to existing mail, Archive all, delayed mail coming back.
Moves made while `serve` is not running, or while the server is unreachable, are
made the next time it can; the sidebar shows how many are pending.

`sync` also cleans up the inbox after a message is safely on disk, set by
`after_fetch`:

- `archive` (default) — MOVE to your Archive folder, so INBOX drains
- `delete` — flag `\Deleted` and expunge
- `keep` — touch nothing; tidymail tracks the last UID it saw instead

Replies are also APPENDed to your Sent folder so your phone and Thunderbird see
them.

**Remove** (`x`) moves a message to the Trash box, and so to the Trash folder on
the server. Locally it stays on disk, so it is never downloaded again.

## Security

- Binds to `127.0.0.1` by default.
- `Content-Security-Policy: default-src 'none'` with `script-src 'self'` and
  `style-src 'self'` — no inline script or style anywhere, no external origins,
  plus `require-trusted-types-for 'script'`. All JS and CSS are separate files
  served from the binary.
- Mail is stored and rendered as plain text and inserted into the DOM as text
  nodes. HTML mail is converted at ingest, with `<script>` and `<style>`
  dropped. The front end never touches `innerHTML`; a test enforces that.
- Attachments are served `Content-Disposition: attachment` with `nosniff` and
  their own sandboxed CSP, and active MIME types are rewritten to
  `application/octet-stream`.
- Every state-changing request needs a same-origin `X-CSRF-Token` header, and
  `Origin`/`Sec-Fetch-Site` are checked. No CORS headers are ever sent.
- All file access under `mail.d` goes through an `os.Root`, so a path in a
  state file cannot escape the directory even via a symlink.

## Development

```sh
make check   # go vet, golangci-lint, go test
```

Two rules the linter enforces rather than trusts:

- **No suppressed errors.** `errcheck` runs with `check-blank: true`, so both
  an unchecked error and `_ = f()` fail the build. Every error is returned to
  the caller or logged.
- **No inline assets.** A test fetches the served page and fails on inline
  `<script>`, inline `style=`, `on*=` handlers, `innerHTML` or `eval`.

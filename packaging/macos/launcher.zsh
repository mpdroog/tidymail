#!/bin/zsh
# Tidymail.app launcher (Contents/MacOS/Tidymail).
#
# Every launch makes sure this Mac is set up, then opens the web UI:
#   1. mail.d lives in ~/Library/Application Support/tidymail; the first launch
#      copies the bundled config.toml there.
#   2. Any account without a password file asks for it once, in a dialog.
#   3. Two launch agents run the tidymail inside this app: sync every minute,
#      and serve permanently. They are rewritten on each launch, so moving or
#      updating the app just works.
#   4. The browser opens http://127.0.0.1:8080.
set -u

contents="${0:A:h:h}"
bin="$contents/Resources/tidymail"
data="$HOME/Library/Application Support/tidymail"
maild="$data/mail.d"
logs="$HOME/Library/Logs/tidymail"
agents="$HOME/Library/LaunchAgents"
url="http://127.0.0.1:8080"
uid=$(id -u)

# fail shows an error dialog and stops; the log says more.
fail() {
  osascript - "$1" "$logs/app.log" <<'EOF' >/dev/null 2>&1
on run argv
  display alert "Tidymail could not start" message (item 1 of argv) & return & return & "Details: " & (item 2 of argv) as critical
end run
EOF
  exit 1
}

mkdir -p "$logs" || exit 1
exec >>"$logs/app.log" 2>&1
print -r -- "--- $(date '+%Y-%m-%d %H:%M:%S') launch from $contents"

[[ -x "$bin" ]] || fail "The tidymail program is missing inside the app."

# Run from the disk image, the launch agents would point into it and stop
# working as soon as it is ejected.
if [[ "$contents" == /Volumes/* ]]; then
  fail "Drag Tidymail onto Applications first, eject the disk image, then open Tidymail from Applications."
fi

# A Mac that already runs tidymail keeps its own mail.d: the launch agents
# name it. Starting a fresh, empty one would hide that mail and its sorting.
existing=$(plutil -extract ProgramArguments.2 raw -o - "$agents/com.mpdroog.tidymail.serve.plist" 2>/dev/null)
if [[ -n "$existing" && "$existing" != "$maild" && -f "$existing/config.toml" ]]; then
  print -r -- "keeping the existing mail.d at $existing"
  maild=$existing
fi
umask 077
mkdir -p "$maild/secrets" "$agents" || fail "Cannot create $maild."

if [[ ! -f "$maild/config.toml" ]]; then
  cp "$contents/Resources/config.toml" "$maild/config.toml" || fail "Cannot write $maild/config.toml."
  print "wrote $maild/config.toml"
fi

# ask_password asks for one account's password with a hidden-answer dialog and
# prints it; it prints nothing when the person cancels.
ask_password() {
  osascript - "$1" "$2" <<'EOF' 2>/dev/null
on run argv
  set theName to item 1 of argv
  set theAddr to item 2 of argv
  try
    set r to display dialog "Password for " & theAddr & " (account " & theName & ")." & return & return & "It is stored on this Mac only, in tidymail's secrets folder." default answer "" with hidden answer with title "Tidymail setup" buttons {"Skip", "Save"} default button "Save"
  on error number -128
    return ""
  end try
  if button returned of r is "Save" then return text returned of r
  return ""
end run
EOF
}

# Every account in config.toml whose password file is missing: name and the
# username right below it.
names=(${(f)"$(sed -n 's/^name = "\(.*\)"$/\1/p' "$maild/config.toml")"})
for name in $names; do
  secret="$maild/secrets/$name.txt"
  [[ -s "$secret" ]] && continue
  addr=$(awk -v n="$name" '$0 == "name = \"" n "\"" {f=1} f && /^username = / {gsub(/^username = "|"$/, ""); print; exit}' "$maild/config.toml")
  pw=$(ask_password "$name" "${addr:-$name}")
  if [[ -z "$pw" ]]; then
    fail "No password was entered for $name. Open Tidymail again to enter it."
  fi
  print -r -- "$pw" > "$secret" || fail "Cannot write the password file for $name."
  unset pw
  print "stored password for $name"
done

# write_agent LABEL COMMAND KEY... writes a launch agent plist for this app.
write_agent() {
  local label=$1 cmd=$2 plist="$agents/$1.plist"
  shift 2
  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$label</string>
	<key>ProgramArguments</key>
	<array>
		<string>$bin</string>
		<string>-dir</string>
		<string>$maild</string>
		<string>$cmd</string>
	</array>
	<key>WorkingDirectory</key>
	<string>$maild</string>
	<key>StandardOutPath</key>
	<string>$logs/$cmd.log</string>
	<key>StandardErrorPath</key>
	<string>$logs/$cmd.log</string>
$*
</dict>
</plist>
EOF
}

write_agent com.mpdroog.tidymail.sync sync \
  "	<key>StartInterval</key><integer>60</integer>
	<key>RunAtLoad</key><true/>
	<key>ProcessType</key><string>Background</string>" || fail "Cannot write the sync launch agent."
write_agent com.mpdroog.tidymail.serve serve \
  "	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>30</integer>" || fail "Cannot write the web UI launch agent."

# Reload both, so they run the tidymail in this copy of the app.
for label in com.mpdroog.tidymail.serve com.mpdroog.tidymail.sync; do
  launchctl bootout "gui/$uid/$label" 2>/dev/null
  # bootout returns before the old job is gone; bootstrap fails until it is.
  loaded=0
  for _ in {1..20}; do
    if launchctl bootstrap "gui/$uid" "$agents/$label.plist"; then
      loaded=1
      break
    fi
    sleep 0.5
  done
  (( loaded )) || fail "Cannot start $label."
done

for _ in {1..40}; do
  curl -s -o /dev/null "$url/" && break
  sleep 0.25
done
curl -s -o /dev/null "$url/" || fail "The web UI did not come up. See $logs/serve.log."
open "$url"

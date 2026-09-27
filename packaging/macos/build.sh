#!/bin/zsh
# Builds Tidymail.app (Apple silicon + Intel) and a DMG to install it with.
#
#   packaging/macos/build.sh CONFIG.toml OUTDIR
#
# CONFIG.toml is the config every new Mac starts with; keep passwords out of it
# (file:secrets/<name>.txt), the app asks for them on first launch.
set -eu

if (( $# != 2 )); then
  print -u2 "usage: $0 CONFIG.toml OUTDIR"
  exit 2
fi
config=${1:A}
out=${2:A}
here=${0:A:h}
repo=${here:h:h}

[[ -f "$config" ]] || { print -u2 "no config at $config"; exit 1; }
if grep -E '^[[:space:]]*password[[:space:]]*=' "$config" | grep -vqE '"(file|env):'; then
  print -u2 "$config holds a literal password; use file:secrets/<name>.txt"
  exit 1
fi

version=$(date +%Y.%m.%d)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
app="$work/dmg/Tidymail.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$out"

print "building tidymail for arm64 and amd64"
for arch in arm64 amd64; do
  (cd "$repo" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -o "$work/tidymail-$arch" ./cmd/tidymail)
done
lipo -create -output "$app/Contents/Resources/tidymail" "$work/tidymail-arm64" "$work/tidymail-amd64"

cp "$here/launcher.zsh" "$app/Contents/MacOS/Tidymail"
chmod 755 "$app/Contents/MacOS/Tidymail" "$app/Contents/Resources/tidymail"
cp "$config" "$app/Contents/Resources/config.toml"

print "drawing the icon"
python3 "$here/make-icon.py" "$work/icon.png"
iconset="$work/tidymail.iconset"
mkdir "$iconset"
for size in 16 32 128 256 512; do
  sips -z $size $size "$work/icon.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
  sips -z $((size * 2)) $((size * 2)) "$work/icon.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns -o "$app/Contents/Resources/tidymail.icns" "$iconset"

cat > "$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>Tidymail</string>
	<key>CFBundleDisplayName</key><string>Tidymail</string>
	<key>CFBundleIdentifier</key><string>com.mpdroog.tidymail</string>
	<key>CFBundleExecutable</key><string>Tidymail</string>
	<key>CFBundleIconFile</key><string>tidymail</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$version</string>
	<key>CFBundleVersion</key><string>$version</string>
	<key>LSMinimumSystemVersion</key><string>12.0</string>
	<key>LSApplicationCategoryType</key><string>public.app-category.productivity</string>
</dict>
</plist>
EOF
plutil -lint "$app/Contents/Info.plist" >/dev/null

# Ad-hoc signature: no Apple developer account, but a consistent, intact bundle.
codesign --force --sign - "$app/Contents/Resources/tidymail"
codesign --force --sign - "$app"

cat > "$work/dmg/Read me.txt" <<'EOF'
Tidymail

1. Drag Tidymail onto Applications.
2. Open Tidymail from Applications. The first time macOS says it cannot check
   the app: open System Settings > Privacy & Security, scroll down and click
   "Open Anyway" next to Tidymail, then confirm.
3. Tidymail asks the password of each mail account once, then opens your mail
   in the browser at http://127.0.0.1:8080. From then on it fetches new mail
   every minute by itself, also after a restart.

Open Tidymail again any time to get back to your mail.

Your mail and settings: ~/Library/Application Support/tidymail
Logs:                   ~/Library/Logs/tidymail
Nothing is moved or deleted on your mail servers.

Uninstall: in Terminal run
  launchctl bootout gui/$(id -u)/com.mpdroog.tidymail.sync
  launchctl bootout gui/$(id -u)/com.mpdroog.tidymail.serve
  rm ~/Library/LaunchAgents/com.mpdroog.tidymail.*.plist
then move Tidymail to the Trash. Remove the Application Support folder too if
you want the downloaded mail gone.
EOF
ln -s /Applications "$work/dmg/Applications"

dmg="$out/Tidymail-$version.dmg"
rm -f "$dmg"
# diskutil image replaces hdiutil create, which on current macOS keeps failing
# with "Resource busy"; older systems without it still use hdiutil.
if diskutil image create from --help >/dev/null 2>&1; then
  diskutil image create from --volumeName Tidymail --format UDZO "$work/dmg" "$dmg" >/dev/null
else
  hdiutil create -volname Tidymail -srcfolder "$work/dmg" -format UDZO -ov -quiet "$dmg"
fi
[[ -f "$dmg" ]] || { print -u2 "could not create $dmg"; exit 1; }
print "built $dmg"

// Package notify raises desktop notifications over the D-Bus session bus.
package notify

import (
	"errors"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
)

// Send shows a desktop notification. There may be no desktop session at all
// (a server, a locked-down cron), so callers should log a failure, not stop.
func Send(summary, body string) (err error) {
	conn, err := sessionBus()
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("notify: close bus: %w", cerr)
		}
	}()

	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"tidymail",                // app name
		uint32(0),                 // replaces id
		"mail-unread",             // icon
		summary,                   // summary
		body,                      // body
		[]string{},                // actions
		map[string]dbus.Variant{}, // hints
		int32(-1),                 // server default timeout
	)
	if call.Err != nil {
		return fmt.Errorf("notify: %w", call.Err)
	}
	return nil
}

// sessionBus connects to the user's session bus. cron does not pass
// DBUS_SESSION_BUS_ADDRESS, so fall back to the standard per-user socket.
func sessionBus() (*dbus.Conn, error) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		conn, err := dbus.ConnectSessionBus()
		if err != nil {
			return nil, fmt.Errorf("connect to session bus: %w", err)
		}
		return conn, nil
	}
	uid := os.Getuid()
	if uid < 0 {
		return nil, errors.New("no DBUS_SESSION_BUS_ADDRESS and no user id on this platform")
	}
	path := fmt.Sprintf("/run/user/%d/bus", uid)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no DBUS_SESSION_BUS_ADDRESS, and %w", err)
	}
	conn, err := dbus.Connect("unix:path=" + path)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", path, err)
	}
	return conn, nil
}

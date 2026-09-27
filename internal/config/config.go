// Package config loads and validates the tidymail configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// TLSMode describes how a connection is secured.
type TLSMode string

// Supported TLS modes.
const (
	TLSImplicit TLSMode = "implicit"
	TLSStartTLS TLSMode = "starttls"
	TLSNone     TLSMode = "none"
)

// AfterFetch describes what happens to a message on the IMAP server once it
// has been stored locally.
type AfterFetch string

// Supported post-fetch actions.
const (
	AfterFetchArchive AfterFetch = "archive"
	AfterFetchDelete  AfterFetch = "delete"
	AfterFetchKeep    AfterFetch = "keep"
)

// IMAP holds the incoming-mail account settings.
type IMAP struct {
	Host        string     `toml:"host"`
	Port        int        `toml:"port"`
	TLS         TLSMode    `toml:"tls"`
	Username    string     `toml:"username"`
	Password    string     `toml:"password"`
	Inbox       string     `toml:"inbox"`
	Sent        string     `toml:"sent"`
	Archive     string     `toml:"archive"`
	Trash       string     `toml:"trash"`
	Junk        string     `toml:"junk"`
	AfterFetch  AfterFetch `toml:"after_fetch"`
	ScanSent    bool       `toml:"scan_sent"`
	MaxFetch    int        `toml:"max_fetch"`
	Connections int        `toml:"connections"` // parallel downloads during sync
}

// SMTP holds the outgoing-mail settings used for replies.
type SMTP struct {
	Host     string  `toml:"host"`
	Port     int     `toml:"port"`
	TLS      TLSMode `toml:"tls"`
	Username string  `toml:"username"`
	Password string  `toml:"password"`
	FromName string  `toml:"from_name"`
	FromAddr string  `toml:"from_addr"`
}

// Summary configures AI summaries, made by running the Claude Code CLI so
// they use its login (a Claude subscription) rather than an API key.
type Summary struct {
	// Command is the claude binary; empty finds it on PATH or in
	// ~/.local/bin, where its installer puts it.
	Command string `toml:"command"`
	// Model is passed as --model when set; empty uses the CLI's default.
	Model string `toml:"model"`
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Listen  string  `toml:"listen"`
	Notify  bool    `toml:"notify"` // desktop notification when sync stores new mail
	IMAP    IMAP    `toml:"imap"`
	SMTP    SMTP    `toml:"smtp"`
	Summary Summary `toml:"summary"`

	// Dir is the mail.d directory this config was loaded from. It is not
	// part of the file itself.
	Dir string `toml:"-"`
}

// Load reads dir/config.toml, resolves secret references and validates the
// result. It never returns a partially validated config.
func Load(dir string) (*Config, error) {
	path := filepath.Join(dir, "config.toml")
	raw, err := os.ReadFile(path) // #nosec G304 -- path is operator supplied
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	cfg := &Config{
		Listen: "127.0.0.1:8080",
		Notify: true,
		Dir:    dir,
	}
	cfg.IMAP = IMAP{Port: 993, TLS: TLSImplicit, Inbox: "INBOX", AfterFetch: AfterFetchArchive, ScanSent: true, MaxFetch: 200, Connections: 3}
	cfg.SMTP = SMTP{Port: 587, TLS: TLSStartTLS}

	md, err := toml.Decode(string(raw), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			if k.String() == "poll_interval" {
				return nil, fmt.Errorf("parse %s: poll_interval was removed: serve no longer fetches mail, run 'tidymail sync' on a timer instead", path)
			}
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("parse %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	cfg.Dir = dir

	if cfg.IMAP.Password, err = resolveSecret(dir, cfg.IMAP.Password); err != nil {
		return nil, fmt.Errorf("imap.password: %w", err)
	}
	if cfg.SMTP.Password, err = resolveSecret(dir, cfg.SMTP.Password); err != nil {
		return nil, fmt.Errorf("smtp.password: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// resolveSecret expands "env:NAME" and "file:PATH" references. Anything else
// is treated as a literal value.
func resolveSecret(dir, v string) (string, error) {
	switch {
	case strings.HasPrefix(v, "env:"):
		name := strings.TrimPrefix(v, "env:")
		got, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %q is not set", name)
		}
		return got, nil
	case strings.HasPrefix(v, "file:"):
		p := strings.TrimPrefix(v, "file:")
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p) // #nosec G304 -- path is operator supplied
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default:
		return v, nil
	}
}

// ErrIncomplete is returned when a mandatory setting is missing.
var ErrIncomplete = errors.New("incomplete configuration")

func (c *Config) validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if c.IMAP.Host == "" {
		return fmt.Errorf("%w: imap.host is required", ErrIncomplete)
	}
	if c.IMAP.Username == "" {
		return fmt.Errorf("%w: imap.username is required", ErrIncomplete)
	}
	if c.IMAP.Password == "" {
		return fmt.Errorf("%w: imap.password is required", ErrIncomplete)
	}
	if err := validPort("imap.port", c.IMAP.Port); err != nil {
		return err
	}
	if err := validTLS("imap.tls", c.IMAP.TLS); err != nil {
		return err
	}
	switch c.IMAP.AfterFetch {
	case AfterFetchArchive, AfterFetchDelete, AfterFetchKeep:
	default:
		return fmt.Errorf("imap.after_fetch %q: want archive, delete or keep", c.IMAP.AfterFetch)
	}
	if c.IMAP.Inbox == "" {
		return fmt.Errorf("%w: imap.inbox is required", ErrIncomplete)
	}
	if c.IMAP.MaxFetch <= 0 {
		return fmt.Errorf("imap.max_fetch must be positive, got %d", c.IMAP.MaxFetch)
	}
	if c.IMAP.Connections < 1 || c.IMAP.Connections > 10 {
		return fmt.Errorf("imap.connections must be between 1 and 10, got %d", c.IMAP.Connections)
	}
	if c.SMTP.Host == "" {
		return fmt.Errorf("%w: smtp.host is required", ErrIncomplete)
	}
	if c.SMTP.FromAddr == "" {
		return fmt.Errorf("%w: smtp.from_addr is required", ErrIncomplete)
	}
	if err := validPort("smtp.port", c.SMTP.Port); err != nil {
		return err
	}
	if err := validTLS("smtp.tls", c.SMTP.TLS); err != nil {
		return err
	}
	return nil
}

func validPort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d is out of range", name, port)
	}
	return nil
}

func validTLS(name string, mode TLSMode) error {
	switch mode {
	case TLSImplicit, TLSStartTLS, TLSNone:
		return nil
	default:
		return fmt.Errorf("%s %q: want implicit, starttls or none", name, mode)
	}
}

// Addr returns host:port for the IMAP account.
func (i IMAP) Addr() string { return net.JoinHostPort(i.Host, strconv.Itoa(i.Port)) }

// Addr returns host:port for the SMTP account.
func (s SMTP) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

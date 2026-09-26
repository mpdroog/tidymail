// Package config loads and validates the tidymail configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
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
	// Mirror repeats box moves in IMAP folders. Off, tidymail never moves
	// mail on the server and boxes exist on this machine only.
	Mirror bool `toml:"mirror"`
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

// Account is one mailbox: where its mail comes from and how replies to it are
// sent. Name tags every message fetched through it, so it must not change once
// mail has been synced.
type Account struct {
	Name string `toml:"name"`
	IMAP IMAP   `toml:"imap"`
	SMTP SMTP   `toml:"smtp"`
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Listen string `toml:"listen"`
	Notify bool   `toml:"notify"` // desktop notification when sync stores new mail

	// IMAP and SMTP are the single-account form of the file. Load turns them
	// into the one entry of Accounts; code outside this package uses Accounts.
	IMAP IMAP `toml:"imap"`
	SMTP SMTP `toml:"smtp"`

	// Accounts holds every mailbox, from [[account]] tables or from the
	// single-account [imap] and [smtp]. The first one owns messages stored
	// without an account name.
	Accounts []Account `toml:"-"`

	// Dir is the mail.d directory this config was loaded from. It is not
	// part of the file itself.
	Dir string `toml:"-"`
}

// Account returns the account a message was fetched through. An empty name
// is the first account: mail stored before accounts had names.
func (c *Config) Account(name string) (Account, bool) {
	if name == "" && len(c.Accounts) > 0 {
		return c.Accounts[0], true
	}
	for i := range c.Accounts {
		if c.Accounts[i].Name == name {
			return c.Accounts[i], true
		}
	}
	return Account{}, false
}

// fileLayout is config.toml as written: [[account]] tables are decoded one by
// one, so each starts from the defaults.
type fileLayout struct {
	*Config
	Accounts []toml.Primitive `toml:"account"`
}

// accountName is what may appear in an account name: it ends up in file names
// inside mail.d.
var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func defaultIMAP() IMAP {
	return IMAP{Port: 993, TLS: TLSImplicit, Inbox: "INBOX", AfterFetch: AfterFetchArchive, ScanSent: true, MaxFetch: 200, Connections: 3, Mirror: true}
}

func defaultSMTP() SMTP { return SMTP{Port: 587, TLS: TLSStartTLS} }

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
	cfg.IMAP = defaultIMAP()
	cfg.SMTP = defaultSMTP()

	file := fileLayout{Config: cfg}
	md, err := toml.Decode(string(raw), &file)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(file.Accounts) > 0 {
		if md.IsDefined("imap") || md.IsDefined("smtp") {
			return nil, fmt.Errorf("parse %s: use either [imap] and [smtp] or [[account]] tables, not both", path)
		}
		for i, prim := range file.Accounts {
			acct := Account{IMAP: defaultIMAP(), SMTP: defaultSMTP()}
			if decErr := md.PrimitiveDecode(prim, &acct); decErr != nil {
				return nil, fmt.Errorf("parse %s: account %d: %w", path, i+1, decErr)
			}
			cfg.Accounts = append(cfg.Accounts, acct)
		}
	} else {
		cfg.Accounts = []Account{{IMAP: cfg.IMAP, SMTP: cfg.SMTP}}
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

	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.IMAP.Password, err = resolveSecret(dir, a.IMAP.Password); err != nil {
			return nil, fmt.Errorf("%simap.password: %w", a.label(), err)
		}
		if a.SMTP.Password, err = resolveSecret(dir, a.SMTP.Password); err != nil {
			return nil, fmt.Errorf("%ssmtp.password: %w", a.label(), err)
		}
	}
	if len(file.Accounts) == 0 {
		// Single-account form: keep IMAP and SMTP resolved as well.
		cfg.IMAP, cfg.SMTP = cfg.Accounts[0].IMAP, cfg.Accounts[0].SMTP
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// label prefixes a setting name in errors so it is clear which account is meant.
func (a Account) label() string {
	if a.Name == "" {
		return ""
	}
	return "account " + a.Name + ": "
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
	named := len(c.Accounts) > 1 || c.Accounts[0].Name != ""
	seen := make(map[string]bool, len(c.Accounts))
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if named {
			if !accountName.MatchString(a.Name) {
				return fmt.Errorf("account %d: name %q: want 1-32 of a-z, 0-9, - and _", i+1, a.Name)
			}
			if seen[a.Name] {
				return fmt.Errorf("account %d: name %q is used twice", i+1, a.Name)
			}
			seen[a.Name] = true
		}
		if err := a.validate(); err != nil {
			return fmt.Errorf("%s%w", a.label(), err)
		}
	}
	return nil
}

func (a Account) validate() error {
	if a.IMAP.Host == "" {
		return fmt.Errorf("%w: imap.host is required", ErrIncomplete)
	}
	if a.IMAP.Username == "" {
		return fmt.Errorf("%w: imap.username is required", ErrIncomplete)
	}
	if a.IMAP.Password == "" {
		return fmt.Errorf("%w: imap.password is required", ErrIncomplete)
	}
	if err := validPort("imap.port", a.IMAP.Port); err != nil {
		return err
	}
	if err := validTLS("imap.tls", a.IMAP.TLS); err != nil {
		return err
	}
	switch a.IMAP.AfterFetch {
	case AfterFetchArchive, AfterFetchDelete, AfterFetchKeep:
	default:
		return fmt.Errorf("imap.after_fetch %q: want archive, delete or keep", a.IMAP.AfterFetch)
	}
	if a.IMAP.Inbox == "" {
		return fmt.Errorf("%w: imap.inbox is required", ErrIncomplete)
	}
	if a.IMAP.MaxFetch <= 0 {
		return fmt.Errorf("imap.max_fetch must be positive, got %d", a.IMAP.MaxFetch)
	}
	if a.IMAP.Connections < 1 || a.IMAP.Connections > 10 {
		return fmt.Errorf("imap.connections must be between 1 and 10, got %d", a.IMAP.Connections)
	}
	if a.SMTP.Host == "" {
		return fmt.Errorf("%w: smtp.host is required", ErrIncomplete)
	}
	if a.SMTP.FromAddr == "" {
		return fmt.Errorf("%w: smtp.from_addr is required", ErrIncomplete)
	}
	if err := validPort("smtp.port", a.SMTP.Port); err != nil {
		return err
	}
	if err := validTLS("smtp.tls", a.SMTP.TLS); err != nil {
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

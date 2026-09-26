package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
[imap]
host = "imap.example.com"
username = "me@example.com"
password = "secret"

[smtp]
host = "smtp.example.com"
from_addr = "me@example.com"
`

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q, want a loopback default", cfg.Listen)
	}
	if cfg.IMAP.Port != 993 || cfg.IMAP.TLS != TLSImplicit {
		t.Errorf("imap defaults = %d/%s", cfg.IMAP.Port, cfg.IMAP.TLS)
	}
	if cfg.IMAP.AfterFetch != AfterFetchArchive || !cfg.IMAP.ScanSent {
		t.Errorf("after_fetch=%s scan_sent=%v", cfg.IMAP.AfterFetch, cfg.IMAP.ScanSent)
	}
	if !cfg.Notify {
		t.Error("notify should default to on")
	}
	if cfg.IMAP.Connections != 3 {
		t.Errorf("imap connections = %d, want 3", cfg.IMAP.Connections)
	}
	if cfg.IMAP.Addr() != "imap.example.com:993" {
		t.Errorf("imap addr = %q", cfg.IMAP.Addr())
	}
}

func TestSecretsResolveFromEnvAndFile(t *testing.T) {
	t.Setenv("TIDYMAIL_TEST_PASSWORD", "from-env")
	dir := write(t, strings.Replace(minimal, `password = "secret"`, `password = "env:TIDYMAIL_TEST_PASSWORD"`, 1))
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IMAP.Password != "from-env" {
		t.Errorf("imap password = %q, want the environment value", cfg.IMAP.Password)
	}

	dir2 := write(t, strings.Replace(minimal, `password = "secret"`, `password = "file:pw.txt"`, 1))
	if writeErr := os.WriteFile(filepath.Join(dir2, "pw.txt"), []byte("from-file\n"), 0o600); writeErr != nil {
		t.Fatalf("write secret: %v", writeErr)
	}
	cfg2, err := Load(dir2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.IMAP.Password != "from-file" {
		t.Errorf("imap password = %q, want the file contents without the newline", cfg2.IMAP.Password)
	}
}

func TestMissingEnvSecretIsAnError(t *testing.T) {
	dir := write(t, strings.Replace(minimal, `password = "secret"`, `password = "env:TIDYMAIL_DEFINITELY_UNSET"`, 1))
	if _, err := Load(dir); err == nil {
		t.Fatal("Load should fail when the referenced variable is unset")
	}
}

func TestValidationRejectsBadInput(t *testing.T) {
	for name, body := range map[string]string{
		"no imap host":    strings.Replace(minimal, `host = "imap.example.com"`, `host = ""`, 1),
		"no smtp from":    strings.Replace(minimal, `from_addr = "me@example.com"`, `from_addr = ""`, 1),
		"bad tls mode":    minimal + "\n[imap]\ntls = \"sometimes\"\n",
		"bad after_fetch": minimal + "\nafter_fetch_typo = 1\n",
		"bad listen":      "listen = \"nonsense\"\n" + minimal,
		"no connections":  strings.Replace(minimal, `username = "me@example.com"`, "username = \"me@example.com\"\nconnections = 0", 1),
		"too many conns":  strings.Replace(minimal, `username = "me@example.com"`, "username = \"me@example.com\"\nconnections = 50", 1),
		"unknown top key": "wat = true\n" + minimal,
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

func TestRemovedPollIntervalPointsAtSync(t *testing.T) {
	_, err := Load(write(t, "poll_interval = \"5m\"\n"+minimal))
	if err == nil || !strings.Contains(err.Error(), "tidymail sync") {
		t.Fatalf("err = %v, want it to point at 'tidymail sync'", err)
	}
}

func TestMissingFileReportsNotExist(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("Load should fail when config.toml is absent")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap os.ErrNotExist so the CLI can suggest 'init'", err)
	}
}

const twoAccounts = `
[[account]]
name = "home"
[account.imap]
host = "imap.home.example"
username = "me@home.example"
password = "secret"
after_fetch = "keep"
[account.smtp]
host = "smtp.home.example"
from_addr = "me@home.example"

[[account]]
name = "work"
[account.imap]
host = "imap.work.example"
username = "me@work.example"
password = "file:work.txt"
[account.smtp]
host = "smtp.work.example"
from_addr = "me@work.example"
`

func TestAccountsDefaultAndResolveIndependently(t *testing.T) {
	dir := write(t, twoAccounts)
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work-secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(cfg.Accounts))
	}
	home, work := cfg.Accounts[0], cfg.Accounts[1]
	if home.Name != "home" || work.Name != "work" {
		t.Errorf("names = %q, %q", home.Name, work.Name)
	}
	// Each [[account]] starts from the defaults, not from the one before it.
	if home.IMAP.AfterFetch != AfterFetchKeep || work.IMAP.AfterFetch != AfterFetchArchive {
		t.Errorf("after_fetch = %s, %s; want keep, archive", home.IMAP.AfterFetch, work.IMAP.AfterFetch)
	}
	if work.IMAP.Port != 993 || work.SMTP.Port != 587 || !work.IMAP.ScanSent {
		t.Errorf("work defaults = %d/%d scan_sent=%v", work.IMAP.Port, work.SMTP.Port, work.IMAP.ScanSent)
	}
	if work.IMAP.Password != "work-secret" {
		t.Errorf("work password = %q, want the file contents", work.IMAP.Password)
	}
	if got, ok := cfg.Account("work"); !ok || got.SMTP.FromAddr != "me@work.example" {
		t.Errorf("Account(work) = %+v, %v", got.SMTP, ok)
	}
	if got, ok := cfg.Account(""); !ok || got.Name != "home" {
		t.Errorf("Account(\"\") = %q, %v; want the first account", got.Name, ok)
	}
	if _, ok := cfg.Account("gone"); ok {
		t.Error("Account(gone) should not be found")
	}
}

func TestSingleAccountFormBecomesOneAccount(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Name != "" || cfg.Accounts[0].IMAP.Host != "imap.example.com" {
		t.Fatalf("accounts = %+v, want the one unnamed account", cfg.Accounts)
	}
}

func TestAccountValidation(t *testing.T) {
	for name, body := range map[string]string{
		"mixed forms":    minimal + twoAccounts,
		"duplicate name": strings.Replace(twoAccounts, `name = "work"`, `name = "home"`, 1),
		"missing name":   strings.Replace(twoAccounts, `name = "work"`, ``, 1),
		"bad name":       strings.Replace(twoAccounts, `name = "work"`, `name = "../work"`, 1),
		"unknown key":    strings.Replace(twoAccounts, `name = "work"`, "name = \"work\"\nnickname = \"w\"", 1),
		"account error":  strings.Replace(twoAccounts, `host = "imap.work.example"`, `host = ""`, 1),
	} {
		dir := write(t, body)
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("pw"), 0o600); err != nil {
			t.Fatalf("write secret: %v", err)
		}
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: Load should have failed", name)
		}
	}
}

func TestMirrorDefaultsOnAndCanBeTurnedOff(t *testing.T) {
	dir := write(t, strings.Replace(twoAccounts, `after_fetch = "keep"`, "after_fetch = \"keep\"\nmirror = false", 1))
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("pw"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Accounts[0].IMAP.Mirror || !cfg.Accounts[1].IMAP.Mirror {
		t.Errorf("mirror = %v, %v; want off for home, the default (on) for work",
			cfg.Accounts[0].IMAP.Mirror, cfg.Accounts[1].IMAP.Mirror)
	}
}

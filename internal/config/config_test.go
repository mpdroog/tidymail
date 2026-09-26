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

package summarize

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a stand-in for the claude CLI that saves its arguments and
// stdin next to itself and prints reply.
func fakeClaude(t *testing.T, reply string) (cmd, dir string) {
	t.Helper()
	dir = t.TempDir()
	cmd = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + filepath.Join(dir, "args") + "\ncat > " + filepath.Join(dir, "stdin") +
		"\ncat <<'REPLY'\n" + reply + "\nREPLY\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return cmd, dir
}

func TestSummarizeRunsClaudeWithoutTools(t *testing.T) {
	cmd, dir := fakeClaude(t, `{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"tldr":" Pay by Friday. ","details":"The invoice is for March."}}`)
	s, err := New(cmd, "some-model", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := s.Summarize(t.Context(), Mail{From: "Ann <ann@example.com>", Subject: "Invoice", Date: time.Now(), Body: "Please pay invoice 42."})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got.TLDR != "Pay by Friday." || got.Details != "The invoice is for March." {
		t.Errorf("summary = %+v", got)
	}

	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	// Each argument is on its own line; the empty ones must be there too.
	for _, want := range []string{"-p\n", "--tools\n\n", "--strict-mcp-config\n", "--setting-sources\n\n",
		"--disable-slash-commands\n", "--no-session-persistence\n", "--model\nsome-model\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("claude was not run with %q; args:\n%s", want, args)
		}
	}
	stdin, err := os.ReadFile(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if !strings.Contains(string(stdin), "Subject: Invoice") || !strings.Contains(string(stdin), "Please pay invoice 42.") {
		t.Errorf("the mail did not reach claude on stdin:\n%s", stdin)
	}
}

func TestSummarizeReportsClaudeErrors(t *testing.T) {
	cmd, _ := fakeClaude(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"usage limit reached"}`)
	s, err := New(cmd, "", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Summarize(t.Context(), Mail{Body: "hi"}); err == nil || !strings.Contains(err.Error(), "usage limit reached") {
		t.Errorf("err = %v, want claude's own message", err)
	}
}

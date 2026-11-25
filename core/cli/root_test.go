package cli

import (
	"bytes"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Execute(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestExitCodes(t *testing.T) {
	if ExitOK != 0 || ExitFailure != 1 || ExitUsage != 2 || ExitNotImplemented != 3 {
		t.Fatal("exit code constants changed")
	}
}

func TestHelpAndUnknown(t *testing.T) {
	code, out, _ := run(t, "--help")
	if code != ExitOK || !strings.Contains(out, "Available Commands") {
		t.Fatalf("help: code=%d out=%q", code, out)
	}
	code, _, errs := run(t, "no-such-command")
	if code != ExitUsage || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown: code=%d err=%q", code, errs)
	}
	code, _, _ = run(t, "version", "--no-such-flag")
	if code != ExitUsage {
		t.Fatalf("bad flag: code=%d", code)
	}
	code, _, errs = run(t, "version", "--output", "xml")
	if code != ExitUsage || !strings.Contains(errs, "output") {
		t.Fatalf("bad output: code=%d err=%q", code, errs)
	}
}

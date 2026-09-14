package launchd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A malformed plist fails at bootstrap time with an unhelpful message, so it
// is worth checking the generated XML parses before it is ever installed.
func TestPlistIsValid(t *testing.T) {
	body := plistBody("/tmp/bin/netwatch", "/tmp/logs", "/tmp/nw.db", "127.0.0.1:7717")

	path := filepath.Join(t.TempDir(), "test.plist")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput()
	if err != nil {
		t.Fatalf("plutil -lint rejected the plist: %v\n%s", err, out)
	}
}

func TestPlistCarriesRequiredKeys(t *testing.T) {
	body := plistBody("/tmp/bin/netwatch", "/tmp/logs", "/tmp/nw.db", "127.0.0.1:7717")

	for _, want := range []string{
		"<string>" + Label + "</string>",
		"<string>/tmp/bin/netwatch</string>",
		"<string>/tmp/nw.db</string>",
		"<string>127.0.0.1:7717</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist is missing %q", want)
		}
	}

	// ProcessType Background throttles timers, which would quietly wreck a
	// sampler's schedule. This must stay Standard.
	if !strings.Contains(body, "<key>ProcessType</key>\n  <string>Standard</string>") {
		t.Error("ProcessType must be Standard, not Background")
	}
}

func TestPathsAreUserScoped(t *testing.T) {
	plist, binary, logDir := Paths()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for name, p := range map[string]string{"plist": plist, "binary": binary, "logDir": logDir} {
		if !strings.HasPrefix(p, home) {
			t.Errorf("%s path %q is outside the user's home; this agent needs no root", name, p)
		}
	}
	if !strings.HasSuffix(plist, ".plist") {
		t.Errorf("plist path %q lacks a .plist suffix", plist)
	}
}

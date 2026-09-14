// Package launchd installs netwatch as a per-user background agent.
package launchd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const Label = "com.kokorhekkus.netwatch"

// Paths returns where the agent's pieces live.
//
// The binary is installed to a fixed location rather than run from the build
// tree. macOS Background Task Management keys its record on the executable's
// path and signature, so a binary that moves or is rebuilt in place can have
// its registration silently invalidated.
func Paths() (plist, binary, logDir string) {
	home, _ := os.UserHomeDir()
	plist = filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
	binary = filepath.Join(home, "Library", "Application Support", "netwatch", "bin", "netwatch")
	logDir = filepath.Join(home, "Library", "Logs", "netwatch")
	return
}

func plistBody(binary, logDir, dbPath, addr string) string {
	// ProcessType is Standard, not Background. Background opts into
	// aggressive CPU *and timer* throttling, which is exactly wrong for a
	// process whose entire job is to sample on a schedule.
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>-db</string>
    <string>%s</string>
    <string>-http</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ProcessType</key>
  <string>Standard</string>
  <key>StandardOutPath</key>
  <string>%s/netwatch.log</string>
  <key>StandardErrorPath</key>
  <string>%s/netwatch.log</string>
</dict>
</plist>
`, Label, binary, dbPath, addr, logDir, logDir)
}

// Install copies the running binary into place and loads the agent.
func Install(dbPath, addr string) (string, error) {
	plistPath, binPath, logDir := Paths()

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running binary: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}

	for _, dir := range []string{filepath.Dir(binPath), logDir, filepath.Dir(plistPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}

	// Unload first: overwriting the binary of a loaded agent leaves launchd
	// pointing at a file that no longer matches its record.
	_ = bootout()

	if self != binPath {
		if err := copyFile(self, binPath); err != nil {
			return "", fmt.Errorf("install binary: %w", err)
		}
	}

	// Ad-hoc sign after copying. An unsigned or stale-signature binary can
	// have its Background Task Management approval silently dropped.
	if out, err := exec.Command("/usr/bin/codesign", "-s", "-", "--force", binPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("codesign: %w: %s", err, strings.TrimSpace(string(out)))
	}

	if err := os.WriteFile(plistPath, []byte(plistBody(binPath, logDir, dbPath, addr)), 0o644); err != nil {
		return "", err
	}

	if err := bootstrap(plistPath); err != nil {
		return "", err
	}
	return plistPath, nil
}

func Uninstall() error {
	plistPath, _, _ := Paths()
	if err := bootout(); err != nil {
		return err
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func bootstrap(plistPath string) error {
	out, err := exec.Command("/bin/launchctl", "bootstrap", domain(), plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func bootout() error {
	// A missing agent is not an error here; this runs on the install path too.
	out, err := exec.Command("/bin/launchctl", "bootout", domain()+"/"+Label).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "No such process") || strings.Contains(msg, "not find") {
			return nil
		}
		return fmt.Errorf("launchctl bootout: %w: %s", err, msg)
	}
	return nil
}

// Status reports whether launchd currently knows about the agent.
func Status() (loaded bool, detail string) {
	out, err := exec.Command("/bin/launchctl", "print", domain()+"/"+Label).CombinedOutput()
	if err != nil {
		return false, "not loaded"
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") || strings.HasPrefix(line, "pid = ") {
			detail += line + "  "
		}
	}
	if detail == "" {
		detail = "loaded"
	}
	return true, strings.TrimSpace(detail)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

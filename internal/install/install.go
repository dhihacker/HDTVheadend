// Package install implements the one-command "-install" / "-uninstall"
// setup: copying the binary to /opt/hdtvheadend, writing a bootstrap admin
// config, and registering a systemd unit.
package install

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"hdtvheadend/internal/config"
	"hdtvheadend/internal/webui"
)

const (
	InstallDir      = "/opt/hdtvheadend"
	BinaryPath      = InstallDir + "/hdtvheadend"
	ConfigPath      = InstallDir + "/config.json"
	SystemdUnitPath = "/etc/systemd/system/hdtvheadend.service"
	ServiceName     = "hdtvheadend"
)

const unitTemplate = `[Unit]
Description=HDTVheadend DVB/IP streaming headend
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -config %s
Restart=on-failure
RestartSec=2
User=root
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW
NoNewPrivileges=false

[Install]
WantedBy=multi-user.target
`

// Run installs HDTVheadend: copies the current executable into InstallDir,
// writes a bootstrap config with a random admin password (printed once),
// installs and starts the systemd unit. Must run as root.
func Run(listenAddr string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("install: must run as root")
	}

	if err := os.MkdirAll(InstallDir, 0o755); err != nil {
		return fmt.Errorf("install: mkdir %s: %w", InstallDir, err)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("install: locate running binary: %w", err)
	}
	if err := copyFile(self, BinaryPath, 0o755); err != nil {
		return fmt.Errorf("install: copy binary: %w", err)
	}

	password, err := randomPassword()
	if err != nil {
		return err
	}
	hash, err := webui.HashPassword(password)
	if err != nil {
		return fmt.Errorf("install: hash password: %w", err)
	}

	if _, err := os.Stat(ConfigPath); os.IsNotExist(err) {
		cfg := config.Default()
		if listenAddr != "" {
			cfg.ListenAddr = listenAddr
		}
		cfg.Admin = config.Admin{Username: "admin", PasswordHash: hash}
		if err := cfg.Save(ConfigPath); err != nil {
			return fmt.Errorf("install: write config: %w", err)
		}
	} else {
		password = "" // existing install: don't print a password that doesn't apply
	}

	unit := fmt.Sprintf(unitTemplate, BinaryPath, ConfigPath)
	if err := os.WriteFile(SystemdUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("install: write systemd unit: %w", err)
	}

	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "--now", ServiceName); err != nil {
		return err
	}

	fmt.Println("HDTVheadend installed and started.")
	if password != "" {
		fmt.Printf("Admin username: admin\nAdmin password: %s\n", password)
		fmt.Println("(shown once — store it now)")
	}
	addr := listenAddr
	if addr == "" {
		addr = config.Default().ListenAddr
	}
	fmt.Printf("Open: http://<this-host>%s\n", addr)
	return nil
}

// RunUninstall stops and removes the systemd unit and installed files.
func RunUninstall() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("uninstall: must run as root")
	}
	_ = run("systemctl", "disable", "--now", ServiceName)
	_ = os.Remove(SystemdUnitPath)
	_ = run("systemctl", "daemon-reload")
	if err := os.RemoveAll(InstallDir); err != nil {
		return fmt.Errorf("uninstall: remove %s: %w", InstallDir, err)
	}
	fmt.Println("HDTVheadend uninstalled.")
	return nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func randomPassword() (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

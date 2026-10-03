// Package remote runs commands and writes files on hosts over SSH.
package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/deemwar-products/chlift/internal/config"
)

// Host runs commands on a server over SSH, or on this machine when the address is "local".
type Host struct {
	Name   string
	client *ssh.Client
	sudo   bool
	local  bool
}

func Dial(c *config.Config, h config.Host) (*Host, error) {
	if h.Address == "local" {
		return &Host{Name: h.Name, local: true}, nil
	}
	var auths []ssh.AuthMethod
	if c.SSH.KeyFile != "" {
		key, err := os.ReadFile(expand(c.SSH.KeyFile))
		if err != nil {
			return nil, err
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.SSH.KeyFile, err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("no SSH key: set ssh.key_file or run an ssh-agent")
	}
	hostKey := ssh.InsecureIgnoreHostKey()
	if !c.SSH.InsecureIgnoreHostKey {
		kh := c.SSH.KnownHosts
		if kh == "" {
			kh = "~/.ssh/known_hosts"
		}
		cb, err := knownhosts.New(expand(kh))
		if err != nil {
			return nil, fmt.Errorf("known_hosts: %w", err)
		}
		hostKey = cb
	}
	addr := net.JoinHostPort(h.Address, strconv.Itoa(h.SSHPort(c)))
	cl, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: c.SSH.User, Auth: auths, HostKeyCallback: hostKey,
		Timeout: 15 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", h.Name, addr, err)
	}
	return &Host{Name: h.Name, client: cl, sudo: c.SSH.User != "root"}, nil
}

func (h *Host) Close() error {
	if h.local {
		return nil
	}
	return h.client.Close()
}

// RunInput is Run with stdin (used to pass request bodies and secrets without putting them on a command line).
func (h *Host) RunInput(ctx context.Context, script string, stdin []byte) (string, error) {
	return h.run(ctx, script, stdin)
}

// Run executes a shell script as root (sudo -n when the SSH user is not root) and returns stdout.
func (h *Host) Run(ctx context.Context, script string) (string, error) {
	return h.run(ctx, script, nil)
}

func (h *Host) run(ctx context.Context, script string, stdin []byte) (string, error) {
	if h.local {
		cmd := exec.CommandContext(ctx, "sh", "-c", "set -e; "+script)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		if err := cmd.Run(); err != nil {
			return out.String(), fmt.Errorf("%s: %w: %s", h.Name, err, lastLines(errb.String(), 8))
		}
		return strings.TrimSpace(out.String()), nil
	}
	s, err := h.client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	var out, errb bytes.Buffer
	s.Stdout, s.Stderr = &out, &errb
	if stdin != nil {
		s.Stdin = bytes.NewReader(stdin)
	}
	cmd := "sh -c " + quote("set -e; "+script)
	if h.sudo {
		cmd = "sudo -n " + cmd
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = s.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	}
	if err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", h.Name, err, lastLines(errb.String(), 8))
	}
	return strings.TrimSpace(out.String()), nil
}

// WriteFile writes content to path (owner root, given mode) only if it differs.
// It reports whether the file changed, which drives restarts.
func (h *Host) WriteFile(ctx context.Context, path string, content []byte, mode os.FileMode, owner string) (bool, error) {
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])
	got, _ := h.Run(ctx, fmt.Sprintf("sha256sum %s 2>/dev/null | cut -d' ' -f1 || true", quote(path)))
	if got == want {
		return false, nil
	}
	tmp := path + ".chlift-tmp"
	script := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %o %s", quote(filepath.Dir(path)), quote(tmp), mode, quote(tmp))
	if owner != "" {
		script += fmt.Sprintf(" && chown %s %s", owner, quote(tmp))
	}
	script += fmt.Sprintf(" && mv %s %s", quote(tmp), quote(path))
	_, err := h.run(ctx, script, content)
	return err == nil, err
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

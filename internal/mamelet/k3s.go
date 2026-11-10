package mamelet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// Where k3s goes. Nothing about these comes from a revision: a revision names only the release
// and the sha256 of its binary.
const (
	k3sBinary = "/usr/local/bin/k3s"
	k3sUnit   = "/etc/systemd/system/k3s.service"
	// maxK3sBinary bounds a download.
	maxK3sBinary = 256 << 20
)

// K3sUnit is the systemd unit mamelet writes (the one the k3s install script writes for a server,
// with K3sArgs).
func K3sUnit() string {
	exec := k3sBinary
	for _, a := range noderev.K3sArgs {
		exec += " " + a
	}
	return `[Unit]
Description=Lightweight Kubernetes (installed by mamelet)
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target

[Install]
WantedBy=multi-user.target

[Service]
Type=notify
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=` + exec + `
`
}

// K3sAsset is the release asset of the architecture mamelet runs on.
func K3sAsset() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "k3s", nil
	case "arm64":
		return "k3s-arm64", nil
	}
	return "", fmt.Errorf("k3s is not supported on %s", runtime.GOARCH)
}

// K3sURL is the download URL of a release's binary.
func K3sURL(base, version string) (string, error) {
	asset, err := K3sAsset()
	if err != nil {
		return "", err
	}
	return base + "/" + noderev.K3sTag(version) + "/" + asset, nil
}

func (s *System) path(p string) string { return filepath.Join(s.Root, p) }

// K3s observes k3s: installed means Mamela's unit is in place and a binary exists.
func (s *System) K3s(ctx context.Context) noderev.K3sState {
	unit, err := os.ReadFile(s.path(k3sUnit))
	if errors.Is(err, os.ErrNotExist) {
		return noderev.K3sState{}
	}
	if err != nil {
		return noderev.K3sState{Error: err.Error()}
	}
	sum, err := fileSHA256(s.path(k3sBinary))
	if errors.Is(err, os.ErrNotExist) {
		return noderev.K3sState{}
	}
	if err != nil {
		return noderev.K3sState{Error: err.Error()}
	}
	if string(unit) != K3sUnit() {
		return noderev.K3sState{SHA256: sum, Error: k3sUnit + " is not the unit mamelet writes (changed by hand?)"}
	}
	return noderev.K3sState{Installed: true, SHA256: sum}
}

// InstallK3s downloads the release's binary, and installs it with Mamela's unit only if its sha256
// is the approved one. It does not start or enable the service (that is a NodeService).
func (s *System) InstallK3s(ctx context.Context, version, wantSHA256 string) error {
	base := s.K3sReleases
	if base == "" {
		base = noderev.K3sReleases
	}
	u, err := K3sURL(base, version)
	if err != nil {
		return err
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", u, resp.StatusCode)
	}
	dir := filepath.Dir(s.path(k3sBinary))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".k3s-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxK3sBinary+1))
	if err == nil && n > maxK3sBinary {
		err = fmt.Errorf("the download is larger than %d MiB", maxK3sBinary>>20)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", u, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return fmt.Errorf("the downloaded k3s %s has sha256 %s, not the approved %s; nothing was installed", version, got, wantSHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path(k3sBinary)); err != nil {
		return err
	}
	if err := writeK3sFile(s.path(k3sUnit), []byte(K3sUnit()), 0o644); err != nil {
		return err
	}
	_, err = s.run(ctx, progSystemctl, []string{"daemon-reload"})
	return err
}

func writeK3sFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mamelet-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

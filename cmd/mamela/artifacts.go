package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/internal/graph"
	"github.com/ieyoukan/mamela/internal/lab"
	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/internal/seed"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// maxDownload bounds a downloaded artifact.
const maxDownload = 8 << 30

// artifacts makes the files providers' changes need (ADR 0004): downloads, checked against their
// sha256 and cached by it, and nodes' mamelet seeds, with the site's network rules.
type artifacts struct {
	Cache string      // where downloads are kept, by sha256
	Site  *lab.Site   // nil: no seeds (no lab, no Control Plane)
	Seeds *seed.Maker // nil: no seeds
	// Nodes gives numbers to the nodes a change leaves without one (ADR 0005).
	Nodes interface {
		NodeNumber(ctx context.Context, node string) (int64, error)
	}
	HTTP *http.Client
}

var _ host.ArtifactMaker = (*artifacts)(nil)

// Make implements host.ArtifactMaker.
func (m *artifacts) Make(ctx context.Context, a protocol.Artifact) (host.Prepared, error) {
	if err := protocol.CheckArtifacts(protocol.Change{Artifacts: []protocol.Artifact{a}}); err != nil {
		return host.Prepared{}, err
	}
	switch a.Kind {
	case protocol.ArtifactDownload:
		path, size, err := m.download(ctx, a)
		if err != nil {
			return host.Prepared{}, err
		}
		return host.Prepared{Info: protocol.ArtifactInfo{Name: a.Name, SHA256: a.SHA256, Size: size},
			Open: func() (io.ReadCloser, error) { return m.verified(path, a.SHA256) }}, nil
	case protocol.ArtifactSeed:
		if m.Seeds == nil || m.Site == nil {
			return host.Prepared{}, errors.New("this Mamela makes no seeds: the site has no Control Plane ([controlplane])")
		}
		network := graphNetwork(*a.Network)
		if err := m.Site.Allows(a.Node, network); err != nil {
			return host.Prepared{}, err
		}
		id := a.NodeID
		if id == 0 {
			if m.Nodes == nil {
				return host.Prepared{}, errors.New("this Mamela gives no node numbers")
			}
			var err error
			if id, err = m.Nodes.NodeNumber(ctx, a.Node); err != nil {
				return host.Prepared{}, err
			}
		}
		var name string
		var img []byte
		var err error
		switch a.Format {
		case protocol.SeedUserData:
			sum := sha256.Sum256(m.Seeds.MameletBinary)
			url := strings.TrimRight(m.Site.ControlPlane.Advertised(), "/") + "/v1/mamelet/" + hex.EncodeToString(sum[:])
			name, img, err = m.Seeds.NewUserData(ctx, a.Node, int(id), network, url)
		default:
			name, img, err = m.Seeds.New(ctx, a.Node, int(id), network)
		}
		if err != nil {
			return host.Prepared{}, err
		}
		sum := sha256.Sum256(img)
		return host.Prepared{Info: protocol.ArtifactInfo{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(img)), NodeID: id},
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(img)), nil }}, nil
	}
	return host.Prepared{}, fmt.Errorf("artifact %s: unknown kind %q", a.ID, a.Kind)
}

func graphNetwork(n protocol.Network) graph.Network {
	return graph.Network{Mode: graph.NetworkMode(n.Mode), Address: n.Address, Gateway: n.Gateway, DNS: n.DNS}
}

// download returns the cached file of the artifact, downloading it first if needed.
func (m *artifacts) download(ctx context.Context, a protocol.Artifact) (string, int64, error) {
	path := filepath.Join(m.Cache, a.SHA256)
	if f, err := m.verified(path, a.SHA256); err == nil {
		f.Close()
		fi, err := os.Stat(path)
		if err != nil {
			return "", 0, err
		}
		return path, fi.Size(), nil
	}
	if err := os.MkdirAll(m.Cache, 0o700); err != nil {
		return "", 0, err
	}
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: time.Hour}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download %s: %w", a.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download %s: %s", a.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(m.Cache, "download-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxDownload+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return "", 0, fmt.Errorf("download %s: %w", a.URL, err)
	case n > maxDownload:
		return "", 0, fmt.Errorf("download %s: larger than %d GiB", a.URL, maxDownload>>30)
	case hex.EncodeToString(h.Sum(nil)) != a.SHA256:
		return "", 0, fmt.Errorf("download %s: sha256 is %x, want %s", a.URL, h.Sum(nil), a.SHA256)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", 0, err
	}
	return path, n, nil
}

// verified opens a cached file after checking its sha256 again.
func (m *artifacts) verified(path, want string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		f.Close()
		return nil, fmt.Errorf("cached %s: sha256 is %s, want %s (delete it to download again)", path, got, want)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// siteLimits is the site's budget for one machine, as limits on providers' measured parameters.
func siteLimits(site *lab.Site) map[string]int64 {
	if site == nil {
		return nil
	}
	return map[string]int64{protocol.MeasureCPU: int64(site.Budget.MaxVCPUPerVM),
		protocol.MeasureMemoryMiB: site.Budget.MaxMemoryMiBPerVM, protocol.MeasureDiskGiB: site.Budget.MaxDiskGiBPerVM}
}

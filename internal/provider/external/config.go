package external

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ieyoukan/mamela/internal/provider/host"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Config says, for each provider, where its API is and how Mamela authenticates to it
// (mamela.providers.toml). It belongs to the user, not to the provider: the provider never sees
// the URL or the credentials.
//
//	[provider.kv-example]
//	api_url    = "https://kv.example.com/api"
//	token_file = "~/.config/kv/token"   # sent as "Authorization: Bearer <token>"
//	catalog    = "kv-catalog.json"      # the API's catalog: the effect of each call
//
//	[provider.pve-vm]
//	api_url         = "https://pve.example:8006/api2/json"
//	auth            = "proxmox"          # "Authorization: PVEAPIToken=<token_id>=<token>"
//	token_id        = "mamela@pve!lab"
//	token_file      = "~/.config/mamela/pve-token"
//	tls_fingerprint = "AA:BB:..."        # pin the server certificate (SHA-256)
//	catalog         = "builtin:proxmox"  # a catalog Mamela ships
type Config struct {
	Provider map[string]APIConfig `toml:"provider"`
	dir      string
}

// APIConfig is one provider's API.
type APIConfig struct {
	APIURL    string `toml:"api_url"`
	TokenFile string `toml:"token_file"`
	Catalog   string `toml:"catalog"`
	// Auth is how the token is sent: "bearer" (the default) or "proxmox" (with TokenID).
	Auth    string `toml:"auth"`
	TokenID string `toml:"token_id"`
	// TLSFingerprint pins the server certificate's SHA-256 ("AA:BB:..."), for an API with a
	// self-signed certificate. Otherwise the system's CAs verify it.
	TLSFingerprint string `toml:"tls_fingerprint"`
}

// LoadConfig reads the configuration. A missing file configures nothing.
func LoadConfig(path string) (Config, error) {
	c := Config{dir: filepath.Dir(path)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return c, fmt.Errorf("%s: unknown keys %v", path, un)
	}
	for name, a := range c.Provider {
		if err := host.CheckBase(a.APIURL); err != nil {
			return c, fmt.Errorf("%s: provider %s: %w", path, name, err)
		}
		switch {
		case a.Auth != "" && a.Auth != "bearer" && a.Auth != "proxmox":
			return c, fmt.Errorf("%s: provider %s: auth %q (bearer or proxmox)", path, name, a.Auth)
		case a.Auth == "proxmox" && (a.TokenID == "" || a.TokenFile == ""):
			return c, fmt.Errorf("%s: provider %s: auth proxmox needs token_id and token_file", path, name)
		}
		if a.TLSFingerprint != "" {
			if _, err := fingerprint(a.TLSFingerprint); err != nil {
				return c, fmt.Errorf("%s: provider %s: %w", path, name, err)
			}
		}
	}
	return c, nil
}

// api returns the upstream and the catalog of a provider, and the endpoint's identity (a digest of
// the URL, the credential and the catalog); nil upstream when it is not configured.
func (c Config) api(name string) (host.Upstream, protocol.Catalog, string, error) {
	a, ok := c.Provider[name]
	if !ok {
		return nil, protocol.Catalog{}, "", nil
	}
	var catalog protocol.Catalog
	if name, ok := strings.CutPrefix(a.Catalog, "builtin:"); ok {
		c, found := Catalogs[name]
		if !found {
			return nil, catalog, "", fmt.Errorf("provider %s: Mamela has no catalog %q", name, a.Catalog)
		}
		catalog = c
	} else if a.Catalog != "" {
		data, err := os.ReadFile(c.resolve(a.Catalog))
		if err != nil {
			return nil, catalog, "", fmt.Errorf("provider %s: catalog: %w", name, err)
		}
		if err := json.Unmarshal(data, &catalog, json.RejectUnknownMembers(true)); err != nil {
			return nil, catalog, "", fmt.Errorf("provider %s: catalog %s: %w", name, a.Catalog, err)
		}
		if err := catalog.Check(); err != nil {
			return nil, catalog, "", err
		}
	}
	var token string
	if a.TokenFile != "" {
		path := c.resolve(a.TokenFile)
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			return nil, catalog, "", fmt.Errorf("provider %s: token file %s is readable by group or others (mode %v); run: chmod 600 %s", name, path, fi.Mode().Perm(), path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, catalog, "", fmt.Errorf("provider %s: token: %w", name, err)
		}
		token = strings.TrimSpace(string(data))
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	if a.TLSFingerprint != "" {
		want, _ := fingerprint(a.TLSFingerprint)
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12,
			// The chain is not verified against a CA; the leaf certificate must match the pin.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("the server sent no certificate")
				}
				if got := sha256.Sum256(cs.PeerCertificates[0].Raw); hex.EncodeToString(got[:]) != want {
					return errors.New("the server certificate does not match the pinned fingerprint")
				}
				return nil
			}}}
	}
	authorization := ""
	switch {
	case token == "":
	case a.Auth == "proxmox":
		authorization = "PVEAPIToken=" + a.TokenID + "=" + token
	default:
		authorization = "Bearer " + token
	}
	up := host.HTTP{Base: strings.TrimRight(a.APIURL, "/"), Client: client,
		Authorize: func(r *http.Request) {
			if authorization != "" {
				r.Header.Set("Authorization", authorization)
			}
		}}
	return up, catalog, endpointID(up.Base, a.Auth+"\x00"+a.TokenID+"\x00"+token+"\x00"+strings.ToLower(a.TLSFingerprint), catalog), nil
}

// fingerprint parses "AA:BB:..." (32 bytes) into lowercase hex.
func fingerprint(s string) (string, error) {
	h := strings.ToLower(strings.ReplaceAll(s, ":", ""))
	if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("tls_fingerprint %q is not a SHA-256 fingerprint", s)
	}
	return h, nil
}

// endpointID identifies an API endpoint: the URL, the credential and the catalog. The credential
// enters only as a digest; a different account or token is a different endpoint.
func endpointID(base, token string, catalog protocol.Catalog) string {
	cred := ""
	if token != "" {
		sum := sha256.Sum256([]byte("mamela-credential\x00" + token))
		cred = "sha256:" + hex.EncodeToString(sum[:])
	}
	data, _ := json.Marshal(struct {
		URL        string           `json:"url"`
		Credential string           `json:"credential"`
		Catalog    protocol.Catalog `json:"catalog"`
	}{base, cred, catalog}, json.Deterministic(true))
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (c Config) resolve(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

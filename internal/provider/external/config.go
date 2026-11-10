package external

import (
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
type Config struct {
	Provider map[string]APIConfig `toml:"provider"`
	dir      string
}

// APIConfig is one provider's API.
type APIConfig struct {
	APIURL    string `toml:"api_url"`
	TokenFile string `toml:"token_file"`
	Catalog   string `toml:"catalog"`
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
	}
	return c, nil
}

// api returns the upstream and the catalog of a provider; nil upstream when it is not configured.
func (c Config) api(name string) (host.Upstream, protocol.Catalog, error) {
	a, ok := c.Provider[name]
	if !ok {
		return nil, protocol.Catalog{}, nil
	}
	var catalog protocol.Catalog
	if a.Catalog != "" {
		data, err := os.ReadFile(c.resolve(a.Catalog))
		if err != nil {
			return nil, catalog, fmt.Errorf("provider %s: catalog: %w", name, err)
		}
		if err := json.Unmarshal(data, &catalog, json.RejectUnknownMembers(true)); err != nil {
			return nil, catalog, fmt.Errorf("provider %s: catalog %s: %w", name, a.Catalog, err)
		}
		if err := catalog.Check(); err != nil {
			return nil, catalog, err
		}
	}
	var token string
	if a.TokenFile != "" {
		data, err := os.ReadFile(c.resolve(a.TokenFile))
		if err != nil {
			return nil, catalog, fmt.Errorf("provider %s: token: %w", name, err)
		}
		token = strings.TrimSpace(string(data))
	}
	up := host.HTTP{Base: strings.TrimRight(a.APIURL, "/"), Client: &http.Client{Timeout: 2 * time.Minute},
		Authorize: func(r *http.Request) {
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
		}}
	return up, catalog, nil
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

// Package kvapi is a small key-value HTTP API for the example provider: items with a value, an
// owner, labels and a version. It stands in for a real API in the provider's tests.
package kvapi

import (
	"encoding/json/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// API names the API in manifests and catalogs.
const API = "kv-example/v1"

// Catalog is what the host knows about the API: the effect of each call. For an API Mamela
// ships a catalog for, it comes from Mamela; for a new API, its author proposes one.
var Catalog = protocol.Catalog{API: API, Entries: []protocol.Entry{
	{Method: "GET", Path: "/items", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/items/{item}", Effect: protocol.EffectRead},
	{Method: "PUT", Path: "/items/{item}", Effect: protocol.EffectChange, Precondition: "version"},
	{Method: "PUT", Path: "/items/{item}/labels", Effect: protocol.EffectChange},
	{Method: "DELETE", Path: "/items/{item}", Effect: protocol.EffectLoseData},
}}

// Item is one item.
type Item struct {
	Name    string   `json:"name"`
	Value   string   `json:"value"`
	Owner   string   `json:"owner,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Version int      `json:"version"`
}

// Server is the API, in memory.
type Server struct {
	mu    sync.Mutex
	items map[string]*Item
}

// New returns an empty API.
func New() *Server { return &Server{items: map[string]*Item{}} }

// Items returns a copy of the items.
func (s *Server) Items() map[string]Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Item{}
	for k, v := range s.items {
		out[k] = *v
	}
	return out
}

// Put stores an item as is (for tests that start from an existing world).
func (s *Server) Put(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[it.Name] = &it
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		var out []Item
		for _, it := range s.items {
			out = append(out, *it)
		}
		slices.SortFunc(out, func(a, b Item) int { return strings.Compare(a.Name, b.Name) })
		reply(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /items/{item}", func(w http.ResponseWriter, r *http.Request) {
		it, ok := s.items[r.PathValue("item")]
		if !ok {
			reply(w, http.StatusNotFound, map[string]string{"error": "no such item"})
			return
		}
		reply(w, http.StatusOK, it)
	})
	mux.HandleFunc("PUT /items/{item}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("item")
		it, exists := s.items[name]
		if v := r.PostFormValue("version"); v != "" && (!exists || strconv.Itoa(it.Version) != v) {
			reply(w, http.StatusConflict, map[string]string{"error": "version mismatch"})
			return
		}
		if !exists {
			it = &Item{Name: name, Owner: r.PostFormValue("owner")}
			s.items[name] = it
		}
		it.Value = r.PostFormValue("value")
		it.Version++
		reply(w, http.StatusOK, it)
	})
	mux.HandleFunc("PUT /items/{item}/labels", func(w http.ResponseWriter, r *http.Request) {
		it, ok := s.items[r.PathValue("item")]
		if !ok {
			reply(w, http.StatusNotFound, map[string]string{"error": "no such item"})
			return
		}
		r.ParseForm()
		it.Labels = slices.Sorted(slices.Values(r.PostForm["labels"]))
		it.Version++
		reply(w, http.StatusOK, it)
	})
	mux.HandleFunc("DELETE /items/{item}", func(w http.ResponseWriter, r *http.Request) {
		delete(s.items, r.PathValue("item"))
		reply(w, http.StatusOK, map[string]string{})
	})
	mux.ServeHTTP(w, r)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.MarshalWrite(w, v, json.Deterministic(true))
}

// Package fakeec2 is a small fake of the EC2 query API calls the example provider uses, for its
// tests: instances in memory that start at once, RunInstances idempotent by client token (as EC2
// is), XML responses, and MinCount and MaxCount held to 1.
package fakeec2

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// Instance is one fake instance.
type Instance struct {
	ID, ImageID, Type, ClientToken, State string
	UserData                              string // decoded
	Tags                                  map[string]string
}

// Server is the fake API.
type Server struct {
	mu        sync.Mutex
	instances []*Instance
}

// New returns a fake with no instances.
func New() *Server { return &Server{} }

// Instances returns a copy of the instances.
func (s *Server) Instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Instance
	for _, i := range s.instances {
		out = append(out, *i)
	}
	return out
}

type xmlTag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

type xmlInstance struct {
	InstanceID   string `xml:"instanceId"`
	ImageID      string `xml:"imageId"`
	InstanceType string `xml:"instanceType"`
	ClientToken  string `xml:"clientToken"`
	LaunchTime   string `xml:"launchTime"`
	State        struct {
		Name string `xml:"name"`
	} `xml:"instanceState"`
	Tags []xmlTag `xml:"tagSet>item"`
}

func (i *Instance) xml() xmlInstance {
	x := xmlInstance{InstanceID: i.ID, ImageID: i.ImageID, InstanceType: i.Type, ClientToken: i.ClientToken, LaunchTime: "2026-11-11T00:00:00Z"}
	x.State.Name = i.State
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range i.Tags {
			if !yield(k) {
				return
			}
		}
	}) {
		x.Tags = append(x.Tags, xmlTag{k, i.Tags[k]})
	}
	return x
}

func fail(w http.ResponseWriter, code, msg string) {
	w.WriteHeader(400)
	fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors></Response>`, code, msg)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "text/xml")
	b, _ := xml.Marshal(v)
	w.Write(b)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method != "POST" || r.URL.Path != "/" || r.ParseForm() != nil {
		fail(w, "InvalidRequest", "POST / with a form")
		return
	}
	f := r.PostForm
	switch f.Get("Action") {
	case "DescribeInstances":
		type resp struct {
			XMLName      xml.Name `xml:"DescribeInstancesResponse"`
			Reservations []struct {
				Instances []xmlInstance `xml:"instancesSet>item"`
			} `xml:"reservationSet>item"`
		}
		var out resp
		for _, i := range s.instances {
			if !match(i, f.Get("Filter.1.Name"), []string{f.Get("Filter.1.Value.1")}) ||
				(f.Get("Filter.2.Name") != "" && !match(i, f.Get("Filter.2.Name"), values(f, "Filter.2.Value."))) {
				continue
			}
			out.Reservations = append(out.Reservations, struct {
				Instances []xmlInstance `xml:"instancesSet>item"`
			}{[]xmlInstance{i.xml()}})
		}
		reply(w, out)
	case "RunInstances":
		if f.Get("MinCount") != "1" || f.Get("MaxCount") != "1" {
			fail(w, "InvalidParameterValue", "the fake launches one instance at a time")
			return
		}
		for _, i := range s.instances {
			if i.ClientToken != "" && i.ClientToken == f.Get("ClientToken") {
				reply(w, struct {
					XMLName   xml.Name      `xml:"RunInstancesResponse"`
					Instances []xmlInstance `xml:"instancesSet>item"`
				}{Instances: []xmlInstance{i.xml()}})
				return
			}
		}
		ud, err := base64.StdEncoding.DecodeString(f.Get("UserData"))
		if err != nil {
			fail(w, "InvalidParameterValue", "UserData is not base64")
			return
		}
		i := &Instance{ID: fmt.Sprintf("i-%017d", len(s.instances)+1), ImageID: f.Get("ImageId"), Type: f.Get("InstanceType"),
			ClientToken: f.Get("ClientToken"), State: "running", UserData: string(ud), Tags: map[string]string{}}
		for n := 1; f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", n)) != ""; n++ {
			i.Tags[f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", n))] = f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", n))
		}
		s.instances = append(s.instances, i)
		reply(w, struct {
			XMLName   xml.Name      `xml:"RunInstancesResponse"`
			Instances []xmlInstance `xml:"instancesSet>item"`
		}{Instances: []xmlInstance{i.xml()}})
	case "StartInstances", "StopInstances":
		for _, i := range s.instances {
			if i.ID == f.Get("InstanceId.1") {
				i.State = map[string]string{"StartInstances": "running", "StopInstances": "stopped"}[f.Get("Action")]
				reply(w, struct {
					XMLName xml.Name `xml:"StartInstancesResponse"`
				}{})
				return
			}
		}
		fail(w, "InvalidInstanceID.NotFound", "no such instance")
	default:
		fail(w, "InvalidAction", "the fake does not know "+f.Get("Action"))
	}
}

func values(f map[string][]string, prefix string) []string {
	var out []string
	for n := 1; ; n++ {
		v, ok := f[fmt.Sprintf("%s%d", prefix, n)]
		if !ok {
			return out
		}
		out = append(out, v...)
	}
}

func match(i *Instance, filter string, vals []string) bool {
	var got string
	switch {
	case filter == "instance-state-name":
		got = i.State
	case filter == "client-token":
		got = i.ClientToken
	case filter == "instance-id":
		got = i.ID
	case strings.HasPrefix(filter, "tag:"):
		var ok bool
		if got, ok = i.Tags[strings.TrimPrefix(filter, "tag:")]; !ok {
			return false
		}
	default:
		return false
	}
	return slices.Contains(vals, got)
}

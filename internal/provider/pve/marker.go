package pve

import (
	"fmt"
	"strings"
)

// The ownership marker lives in the VM's description. Together with the pool, the VMID range, the
// tag and the VMID reservation in Mamela's database, it decides whether Mamela manages a VM.
const markerHeader = "Managed by Mamela. Do not edit the lines below."

// Marker states. A VM is "creating" from the moment Proxmox writes its configuration until it has
// its full disk and has been started; only then is it "ready". A VM that is not ready is not
// reported as existing, so the next create resumes it instead of making another.
const (
	stateCreating = "creating"
	stateReady    = "ready"
)

type marker struct {
	Name    string
	Image   string
	Network string // graph.Network.String(): how the VM was given its address at creation
	State   string
}

func (m marker) String() string {
	return fmt.Sprintf("%s\nmamela-name: %s\nmamela-image: %s\nmamela-network: %s\nmamela-state: %s\n",
		markerHeader, m.Name, m.Image, m.Network, m.State)
}

// parseMarker reads the marker from a description. ok is false if there is none or it is malformed.
func parseMarker(description string) (marker, bool) {
	var m marker
	header := false
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if line == markerHeader {
			header = true
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "mamela-name":
			m.Name = value
		case "mamela-image":
			m.Image = value
		case "mamela-network":
			m.Network = value
		case "mamela-state":
			m.State = value
		}
	}
	if m.Network == "" {
		m.Network = "dhcp" // VMs made before networks were declared all used DHCP
	}
	ok := header && m.Name != "" && m.Image != "" && (m.State == stateCreating || m.State == stateReady)
	return m, ok
}

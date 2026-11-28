package protocol

import (
	"errors"
	"regexp"
)

// New centers advertise the optional report endpoint during the authenticated
// WebSocket upgrade. Older agents and centers can still exchange tasks.
const HostStatusHeader = "X-PTO-Host-Status"
const MaxServices = 256
const MaxHostStatusBytes = 192 << 10

type ServiceStatus struct {
	Unit   string `json:"unit"`
	Active string `json:"active"`
	Sub    string `json:"sub"`
}

// A nil value means that the corresponding native source was unavailable.
type HostStatus struct {
	LoggedInUsers     *int            `json:"loggedInUsers"`
	Services          []ServiceStatus `json:"services"`
	ServicesTruncated bool            `json:"servicesTruncated,omitempty"`
}

var serviceUnit = regexp.MustCompile(`^[A-Za-z0-9_:@.\\-]+\.service$`)
var serviceState = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func (u ServiceStatus) Valid() bool {
	return len(u.Unit) <= 255 && serviceUnit.MatchString(u.Unit) && serviceState.MatchString(u.Active) && serviceState.MatchString(u.Sub)
}

func (s HostStatus) Validate() error {
	if s.LoggedInUsers != nil && (*s.LoggedInUsers < 0 || *s.LoggedInUsers > 1<<24) {
		return errors.New("invalid login count")
	}
	if len(s.Services) > MaxServices || s.ServicesTruncated && len(s.Services) != MaxServices {
		return errors.New("invalid service count")
	}
	seen := make(map[string]bool, len(s.Services))
	for _, u := range s.Services {
		if !u.Valid() || seen[u.Unit] {
			return errors.New("invalid service status")
		}
		seen[u.Unit] = true
	}
	return nil
}

// Package hoststatus reads aggregate Linux login and system service status.
package hoststatus

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

const Interval = 30 * time.Second

// Read a bounded amount from a fixed local command. Unavailable data stays nil.
func statusOutput(ctx context.Context, binary string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "SYSTEMD_COLORS=0"}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	const limit = 2 << 20
	data, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if readErr != nil || len(data) > limit {
		cancel()
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("host status output exceeds %d bytes", limit)
	}
	return data, readErr
}

func loginCount(raw []byte) *int {
	users := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) != 0 {
			users[fields[0]] = struct{}{}
		}
	}
	n := len(users)
	return &n
}

func servicePriority(u p.ServiceStatus) int {
	switch u.Active {
	case "failed":
		return 0
	case "activating", "deactivating", "reloading", "refreshing":
		return 1
	case "active":
		return 2
	default:
		return 3
	}
}

func serviceList(raw []byte) ([]p.ServiceStatus, bool) {
	units := []p.ServiceStatus{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "●")))
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return nil, false
		}
		u := p.ServiceStatus{Unit: fields[0], Active: fields[2], Sub: fields[3]}
		if !u.Valid() {
			return nil, false
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool {
		a, b := servicePriority(units[i]), servicePriority(units[j])
		if a != b {
			return a < b
		}
		return units[i].Unit < units[j].Unit
	})
	if len(units) > p.MaxServices {
		return units[:p.MaxServices], true
	}
	return units, false
}

func Collect(ctx context.Context) p.HostStatus {
	var status p.HostStatus
	if raw, err := statusOutput(ctx, "/usr/bin/who", "/run/utmp"); err == nil {
		status.LoggedInUsers = loginCount(raw)
	}
	if raw, err := statusOutput(ctx, "/usr/bin/systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager", "--full"); err == nil {
		status.Services, status.ServicesTruncated = serviceList(raw)
	}
	return status
}

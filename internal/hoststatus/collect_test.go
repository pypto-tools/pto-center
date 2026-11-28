package hoststatus

import (
	"fmt"
	"strings"
	"testing"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

func TestLoginAccountsAndServiceStates(t *testing.T) {
	if got := *loginCount([]byte("alice pts/0 10:01\nalice pts/1 10:04\nbob tty1 09:00\n")); got != 2 {
		t.Fatalf("deduplicated login count = %d", got)
	}
	if *loginCount(nil) != 0 {
		t.Fatal("empty successful login listing should have no users")
	}
	raw := "sshd.service loaded active running OpenSSH\n● worker@1.service loaded failed failed worker\n" +
		`cache\x2dmount.service loaded inactive dead Cache` + "\nboot.service loaded active exited Boot\n"
	units, truncated := serviceList([]byte(raw))
	if truncated || len(units) != 4 || units[0].Unit != "worker@1.service" || units[1].Sub != "exited" || units[2].Sub != "running" || units[3].Active != "inactive" {
		t.Fatalf("service listing: %+v, truncated=%v", units, truncated)
	}
	if units, _ := serviceList(nil); units == nil || len(units) != 0 {
		t.Fatal("empty successful listing should be distinct from unavailable")
	}
	if units, _ := serviceList([]byte("malformed output")); units != nil {
		t.Fatal("unrecognized output should leave services unknown")
	}
	var many strings.Builder
	for i := 0; i < p.MaxServices; i++ {
		fmt.Fprintf(&many, "daemon%d.service loaded active running\n", i)
	}
	many.WriteString("late.service loaded failed failed\n")
	units, truncated = serviceList([]byte(many.String()))
	if !truncated || len(units) != p.MaxServices || units[0].Unit != "late.service" {
		t.Fatal("a failed service should remain visible when truncating")
	}
	if err := (p.HostStatus{Services: units, ServicesTruncated: truncated}).Validate(); err != nil {
		t.Fatal(err)
	}
}

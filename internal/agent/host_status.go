package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/pypto-tools/pto-center/internal/hoststatus"
)

func (a Agent) reportHostStatus(ctx context.Context, client *http.Client, key string) error {
	status := hoststatus.Collect(ctx)
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.Base, "/")+"/status", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-PTO-Node", a.Name)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("host status report returned %d", res.StatusCode)
	}
	return nil
}

func (a Agent) monitorHost(ctx context.Context, key string) {
	client := a.client()
	client.Timeout = 5 * time.Second
	defer client.CloseIdleConnections()
	tick := time.NewTicker(hoststatus.Interval)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := a.reportHostStatus(ctx, client, key); err != nil && ctx.Err() == nil {
			log.Printf("node %s: %v", a.Name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

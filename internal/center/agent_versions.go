package center

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

var errAgentTarget = errors.New("Agent 目标不可用")

func reportedAgent(h http.Header) (*p.Inventory, error) {
	version, platform, managed := h.Get(p.AgentVersionHeader), h.Get(p.AgentPlatformHeader), h.Get(p.AgentManagedHeader)
	if version == "" && platform == "" && managed == "" {
		return nil, nil
	}
	if !p.Version.MatchString(version) || !p.AgentPlatform.MatchString(platform) || (managed != "" && managed != "1") {
		return nil, errors.New("invalid agent identity")
	}
	inv := p.AgentInventory(version, platform, managed == "1")
	return &inv, nil
}

func agentObservation(tx *sql.Tx, node string) (*p.Inventory, error) {
	var raw []byte
	err := tx.QueryRow("SELECT raw FROM inventory WHERE node=? AND tool=?", node, p.AgentTool).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var inv p.Inventory
	err = json.Unmarshal(raw, &inv)
	return &inv, err
}

func (s *Store) recordAgentConnection(node, key string, inv *p.Inventory) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ok int
	if err = tx.QueryRow("SELECT 1 FROM nodes WHERE name=? AND credential=?", node, p.Hash([]byte(key))).Scan(&ok); err != nil {
		return err
	}
	if inv == nil {
		_, err = tx.Exec("DELETE FROM inventory WHERE node=? AND tool=?", node, p.AgentTool)
	} else {
		old, err := agentObservation(tx, node)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		if old == nil || inv.Running != old.Running || inv.Platform != old.Platform {
			if _, err := tx.Exec("INSERT INTO agent_versions(node,version,platform,seen) VALUES(?,?,?,?)", node, inv.Running, inv.Platform, now); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(inv)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO inventory(node,tool,raw,seen) VALUES(?,?,?,?)", node, p.AgentTool, raw, now); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

type agentVersion struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Seen     int64  `json:"seen"`
}

func (s *Store) agentVersions(node string) ([]agentVersion, error) {
	rows, err := s.db.Query("SELECT version,platform,seen FROM agent_versions WHERE node=? ORDER BY seq DESC LIMIT 100", node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agentVersion{}
	for rows.Next() {
		var entry agentVersion
		if err := rows.Scan(&entry.Version, &entry.Platform, &entry.Seen); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func agentPlatforms(tx *sql.Tx, tool, digest string) ([]string, error) {
	if tool != p.AgentTool {
		return nil, nil
	}
	var raw []byte
	if err := tx.QueryRow("SELECT meta FROM bundles WHERE tool=? AND digest=?", tool, digest).Scan(&raw); err != nil {
		return nil, err
	}
	var t p.Tool
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	if len(t.Platforms) == 0 {
		return nil, fmt.Errorf("%w：需要原生 pto-agent 发布包", errAgentTarget)
	}
	return t.Platforms, nil
}

func agentTargetReason(inv *p.Inventory, platforms []string, action string) string {
	if inv == nil || inv.Platform == "" {
		return "Agent 尚未上报版本，需要先部署支持自管理的基础版"
	}
	if action == "probe" {
		return ""
	}
	if !slices.Contains(inv.Capabilities, action) {
		return "此 Agent 未启用托管升级"
	}
	if !slices.Contains(platforms, inv.Platform) {
		return "发布包缺少 " + inv.Platform + " 程序"
	}
	return ""
}

func checkAgentTarget(tx *sql.Tx, t p.Task) error {
	if t.Tool != p.AgentTool {
		return nil
	}
	platforms, err := agentPlatforms(tx, t.Tool, t.Bundle)
	if err != nil {
		return err
	}
	inv, err := agentObservation(tx, t.Node)
	if err != nil {
		return err
	}
	if reason := agentTargetReason(inv, platforms, t.Action); reason != "" {
		return fmt.Errorf("%w，%s：%s", errAgentTarget, t.Node, reason)
	}
	return nil
}

package center

import (
	"database/sql"
	"encoding/json"
	"errors"
	"runtime"
	"slices"

	"github.com/pypto-tools/pto-center/internal/buildversion"
	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type centerChangeRequest struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	Bundle   string `json:"bundle"`
	Expected string `json:"expected"`
}

type centerChange struct {
	centerChangeRequest
	Version string `json:"version"`
	State   string `json:"state"`
	Summary string `json:"summary"`
}

const centerFields = "id,action,bundle,previous,target,state,summary"

func readCenterChange(row *sql.Row) (*centerChange, error) {
	var change centerChange
	err := row.Scan(&change.ID, &change.Action, &change.Bundle, &change.Expected, &change.Version, &change.State, &change.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &change, err
}

type centerUpdateInfo struct {
	Running  string        `json:"running"`
	Platform string        `json:"platform"`
	Managed  bool          `json:"managed"`
	Releases []p.Tool      `json:"releases"`
	Last     *centerChange `json:"last,omitempty"`
}

func (s *Server) centerUpdateInfo() (centerUpdateInfo, error) {
	out := centerUpdateInfo{Running: buildversion.Current(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Managed: s.Supervised}
	rows, err := s.Store.db.Query("SELECT meta FROM bundles WHERE tool=? ORDER BY rowid DESC", p.CenterTool)
	if err != nil {
		return out, err
	}
	out.Releases, err = decodeRows[p.Tool](rows)
	if err == nil {
		out.Last, err = readCenterChange(s.Store.db.QueryRow("SELECT " + centerFields + " FROM center_changes ORDER BY seq DESC LIMIT 1"))
	}
	return out, err
}

func (s *Store) queueCenterChange(in centerChangeRequest, running string) (*centerChange, error) {
	if !p.Digest.MatchString(in.ID) || !p.Digest.MatchString(in.Bundle) || !p.Version.MatchString(in.Expected) || (in.Action != "update" && in.Action != "rollback") {
		return nil, errors.New("无效的中心版本切换请求")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	old, err := readCenterChange(tx.QueryRow("SELECT "+centerFields+" FROM center_changes WHERE id=?", in.ID))
	if err != nil {
		return nil, err
	}
	if old != nil {
		if old.centerChangeRequest != in {
			return nil, errors.New("请求编号已用于其他切换")
		}
		return old, nil
	}
	tool, err := registeredTool(tx, in.Bundle)
	if err != nil {
		return nil, err
	}
	if tool.Manifest.ID != p.CenterTool || !slices.Contains(tool.Platforms, runtime.GOOS+"/"+runtime.GOARCH) {
		return nil, errors.New("需要包含本机架构的中心签名包")
	}
	if in.Expected != running || tool.Version == running {
		return nil, errors.New("当前运行版本已改变或已满足，请刷新")
	}
	var busy bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM center_changes WHERE state IN ('queued','running'))").Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, errors.New("另一项中心切换尚未结束")
	}
	change := &centerChange{centerChangeRequest: in, Version: tool.Version, State: "queued", Summary: "等待中心机执行器"}
	if _, err := tx.Exec("INSERT INTO center_changes("+centerFields+") VALUES(?,?,?,?,?,?,?)", in.ID, in.Action, in.Bundle, in.Expected, tool.Version, change.State, change.Summary); err != nil {
		return nil, err
	}
	return change, tx.Commit()
}

func (s *Store) centerImage(digest, version string) ([]byte, error) {
	var raw []byte
	if err := s.db.QueryRow("SELECT raw FROM bundles WHERE digest=? AND tool=?", digest, p.CenterTool).Scan(&raw); err != nil {
		return nil, err
	}
	var b p.Bundle
	if len(raw) > p.MaxAgentBundle || json.Unmarshal(raw, &b) != nil {
		return nil, errors.New("中心签名包损坏")
	}
	tool, err := b.Verify(s.key)
	if err != nil {
		return nil, err
	}
	if tool.Manifest.ID != p.CenterTool || tool.Digest != digest || tool.Version != version {
		return nil, errors.New("中心签名包的组件、摘要或版本不符")
	}
	return b.Release.CenterImage(runtime.GOOS + "/" + runtime.GOARCH)
}

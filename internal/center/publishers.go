package center

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

type PublisherInfo struct {
	p.ToolPublisher
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Checked     int64  `json:"checked"`
	Message     string `json:"message,omitempty"`
	Version     string `json:"version,omitempty"`
	Digest      string `json:"digest,omitempty"`
	assetHash   string
}

type publisherCheck struct {
	p.ToolPublisher
	tag string
}

const publisherCols = "tool,repo,key,state,checked,message,version,digest,asset_hash"

func readPublisher(row interface{ Scan(...any) error }) (PublisherInfo, error) {
	var k PublisherInfo
	err := row.Scan(&k.Tool, &k.Repository, &k.PublicKey, &k.State, &k.Checked, &k.Message, &k.Version, &k.Digest, &k.assetHash)
	if err == nil {
		k.Fingerprint = p.Hash(k.PublicKey)
	}
	return k, err
}

func (s *Store) publishers() ([]PublisherInfo, error) {
	rows, err := s.db.Query("SELECT " + publisherCols + " FROM tool_publishers ORDER BY tool")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []PublisherInfo{}
	for rows.Next() {
		k, err := readPublisher(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, k)
	}
	return list, rows.Err()
}

func samePublisher(a, b *p.ToolPublisher) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Tool == b.Tool && strings.EqualFold(a.Repository, b.Repository) && bytes.Equal(a.PublicKey, b.PublicKey)
}

var errPublisherChanged = errors.New("工具公钥已变更，请刷新后重试")

// The browser sends only a public key. A stale tab cannot replace another
// operator's binding; retrying a previously accepted request is idempotent.
func (s *Store) setPublisher(k p.ToolPublisher, expected string) (PublisherInfo, error) {
	k.Repository = strings.ToLower(k.Repository)
	if !p.Name.MatchString(k.Tool) || k.Tool == p.CenterTool || k.Tool == p.AgentTool || !repoName.MatchString(k.Repository) ||
		len(k.PublicKey) != ed25519.PublicKeySize || bytes.Equal(k.PublicKey, s.key) || (expected != "" && !p.Digest.MatchString(expected)) {
		return PublisherInfo{}, errors.New("请提供业务工具 ID、GitHub owner/repository 和独立的 Ed25519 公钥")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PublisherInfo{}, err
	}
	defer tx.Rollback()
	current, err := readPublisher(tx.QueryRow("SELECT "+publisherCols+" FROM tool_publishers WHERE tool=?", k.Tool))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PublisherInfo{}, err
	}
	if samePublisher(&current.ToolPublisher, &k) {
		return current, nil
	}
	if expected != current.Fingerprint || (current.Tool != "" && !strings.EqualFold(current.Repository, k.Repository)) {
		return PublisherInfo{}, errPublisherChanged
	}
	var other bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM tool_publishers WHERE key=? AND tool<>?)", k.PublicKey, k.Tool).Scan(&other); err != nil {
		return PublisherInfo{}, err
	}
	if other {
		return PublisherInfo{}, errors.New("该公钥已绑定其他工具")
	}
	_, err = tx.Exec(`INSERT INTO tool_publishers(tool,repo,key) VALUES(?,?,?) ON CONFLICT(tool) DO UPDATE SET
		key=excluded.key,state='pending',checked=0,message='',version='',digest='',asset_hash=''`, k.Tool, k.Repository, k.PublicKey)
	if err != nil {
		return PublisherInfo{}, err
	}
	return PublisherInfo{ToolPublisher: k, Fingerprint: p.Hash(k.PublicKey), State: "pending"}, tx.Commit()
}

// Existing releases retain their original signing binding. A new release of a
// bound tool must always verify with its current repository key.
func (s *Store) verifyIncoming(tx *sql.Tx, b p.Bundle, size int, check *publisherCheck) (p.Tool, error) {
	var hint struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b.Release.Manifest, &hint); err != nil {
		return p.Tool{}, err
	}
	if hint.ID == p.AgentTool || hint.ID == p.CenterTool {
		if check != nil {
			return p.Tool{}, errors.New("仓库发布包不能指定平台组件")
		}
		return b.Verify(s.key)
	}
	if size > p.MaxBundle {
		return p.Tool{}, errors.New("bundle exceeds limit")
	}
	if check == nil {
		var meta []byte
		err := tx.QueryRow("SELECT meta FROM bundles WHERE tool=? AND version=?", hint.ID, b.Release.Version).Scan(&meta)
		if err == nil {
			var prior p.Tool
			if err = json.Unmarshal(meta, &prior); err != nil {
				return p.Tool{}, err
			}
			if prior.Publisher != nil {
				return prior.Publisher.Verify(b)
			}
			return b.Verify(s.key)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return p.Tool{}, err
		}
	}
	current, err := readPublisher(tx.QueryRow("SELECT "+publisherCols+" FROM tool_publishers WHERE tool=?", hint.ID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p.Tool{}, err
	}
	if check != nil && (check.tag != b.Release.Version || !samePublisher(&current.ToolPublisher, &check.ToolPublisher)) {
		return p.Tool{}, errors.New("发布包的工具、标签或登记公钥不匹配")
	}
	if current.Tool != "" {
		return current.Verify(b)
	}
	return b.Verify(s.key)
}

package center

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

// Keep the original scalar as the first membership so existing databases,
// older CLI invitations and API readers retain the original label verbatim.
const nodeGroupsJSON = `(SELECT json_group_array(label) FROM (
	SELECT nodes.group_name AS label, 0 AS position WHERE nodes.group_name<>''
	UNION ALL SELECT label,position FROM node_extra_groups WHERE node=nodes.name
	ORDER BY position))`

var errGroupsChanged = errors.New("分组已被修改，请关闭窗口，刷新主机清单后重试")

func writeGroups(tx *sql.Tx, name string, groups []string) error {
	seen := make(map[string]bool)
	labels := make([]string, 0, len(groups))
	for _, label := range groups {
		if len(label) > 80 {
			return errors.New("每个分组名称最多 80 字节")
		}
		if label != "" && !seen[label] {
			labels = append(labels, label)
			seen[label] = true
		}
	}
	first := ""
	if len(labels) > 0 {
		first = labels[0]
	}
	if _, err := tx.Exec("UPDATE nodes SET group_name=? WHERE name=?", first, name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM node_extra_groups WHERE node=?", name); err != nil {
		return err
	}
	for i := 1; i < len(labels); i++ {
		if _, err := tx.Exec("INSERT INTO node_extra_groups(node,position,label) VALUES(?,?,?)", name, i, labels[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Invite(name string, groups ...string) (string, error) {
	if !p.Name.MatchString(name) {
		return "", errors.New("invalid node name")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	token := Token()
	if _, err := tx.Exec("INSERT INTO nodes(name,invitation) VALUES(?,?)", name, p.Hash([]byte(token))); err != nil {
		return "", err
	}
	if err := writeGroups(tx, name, groups); err != nil {
		return "", err
	}
	return token, tx.Commit()
}

func (s *Store) SetGroups(name string, expected, groups []string) error {
	if expected == nil || groups == nil {
		return errors.New("请提供原分组和新分组列表")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err := tx.QueryRow("SELECT "+nodeGroupsJSON+" FROM nodes WHERE name=?", name).Scan(&raw); err != nil {
		return err
	}
	var current []string
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	if !slices.Equal(expected, current) {
		return errGroupsChanged
	}
	if err := writeGroups(tx, name, groups); err != nil {
		return err
	}
	return tx.Commit()
}

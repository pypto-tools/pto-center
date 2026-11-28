package center

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

// Register the two artifacts of one published platform version atomically.
// Downloading and registering a release never dispatches an update.
func (s *Server) syncPlatformRelease(ctx context.Context) ([]p.Tool, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	const releases = "https://api.github.com/repos/pypto-tools/pto-center/releases"
	raw, err := s.github(ctx, releases+"/latest", "application/vnd.github+json", 2<<20)
	if err != nil {
		return nil, err
	}
	var release struct {
		Tag               string `json:"tag_name"`
		Draft, Prerelease bool
		Assets            []struct {
			ID   int64
			Name string
		}
	}
	if json.Unmarshal(raw, &release) != nil || release.Draft || release.Prerelease || !strings.HasPrefix(release.Tag, "v") || !p.Version.MatchString(release.Tag) {
		return nil, errors.New("GitHub 未返回有效的正式平台版本")
	}
	assets := map[string]int64{}
	for _, a := range release.Assets {
		assets[a.Name] = a.ID
	}
	var tools []p.Tool
	var files [][]byte
	for _, name := range []string{p.CenterTool, p.AgentTool} {
		filename := name + "-" + release.Tag + ".json"
		id := assets[filename]
		if id <= 0 {
			return nil, fmt.Errorf("发行版缺少 %s", filename)
		}
		raw, err := s.githubDownload(ctx, releases+"/assets/"+strconv.FormatInt(id, 10), "application/octet-stream", p.MaxAgentBundle, true)
		if err != nil {
			return nil, err
		}
		var b p.Bundle
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		tool, err := b.Verify(s.Store.key)
		if err != nil {
			return nil, err
		}
		if tool.Manifest.ID != name || tool.Version != release.Tag {
			return nil, fmt.Errorf("%s 的签名内容与组件或标签不符", filename)
		}
		tools, files = append(tools, tool), append(files, raw)
	}
	tx, err := s.Store.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i, tool := range tools {
		if err := registerBundle(tx, files[i], tool); err != nil {
			return nil, err
		}
	}
	return tools, tx.Commit()
}

type SourceRecord struct {
	generation int64
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Tool       string `json:"tool"`
	Commit     string `json:"commit"`
	Checked    int64  `json:"checked"`
	Message    string `json:"message"`
}

func (s *Server) sources() ([]SourceRecord, error) {
	rows, err := s.Store.db.Query("SELECT rowid,repo,path,tool,revision,checked,message FROM sources ORDER BY repo,path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SourceRecord{}
	for rows.Next() {
		var r SourceRecord
		if err = rows.Scan(&r.generation, &r.Repository, &r.Path, &r.Tool, &r.Commit, &r.Checked, &r.Message); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

var errSourceChanged = errors.New("source changed during synchronization")

func (s *Server) registerSource(raw []byte, repo, commit, path string, expected *SourceRecord) (p.Tool, error) {
	if repo == "" {
		return s.Store.Register(raw)
	}
	return s.Store.register(raw, &SourceRecord{Repository: repo, Path: path, Commit: commit}, expected)
}
func (s *Server) syncOne(ctx context.Context, r SourceRecord) error {
	raw, err := s.github(ctx, "https://api.github.com/repos/"+r.Repository+"/commits?per_page=1&path="+url.QueryEscape(r.Path), "application/vnd.github+json", 128<<10)
	if err != nil {
		return err
	}
	var commits []struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(raw, &commits) != nil || len(commits) == 0 || !commitName.MatchString(commits[0].SHA) {
		return errors.New("no valid commit for registered bundle")
	}
	revision := commits[0].SHA
	if revision == r.Commit {
		return nil
	}
	raw, err = s.githubBundle(ctx, r.Repository, revision, r.Path)
	if err != nil {
		return err
	}
	_, err = s.registerSource(raw, r.Repository, revision, r.Path, &r)
	if err == nil {
		s.wake()
	}
	return err
}

// Sync discovers signed candidates. Only an operator-published task can install one.
func (s *Server) Sync(ctx context.Context) ([]SourceRecord, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	sources, err := s.sources()
	if err != nil {
		return nil, err
	}
	for _, r := range sources {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		message := "已同步"
		if syncErr := s.syncOne(ctx, r); syncErr != nil {
			message = syncErr.Error()
		}
		if _, err = s.Store.db.Exec("UPDATE sources SET checked=?,message=? WHERE repo=? AND path=? AND rowid=? AND revision=?", time.Now().Unix(), message, r.Repository, r.Path, r.generation, r.Commit); err != nil {
			return nil, err
		}
	}
	return s.sources()
}
func (s *Server) WatchSources(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.Sync(ctx)
		}
	}
}

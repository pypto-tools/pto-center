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

type githubAsset struct {
	ID     int64
	Name   string
	Digest string
}

type githubRelease struct {
	Tag               string `json:"tag_name"`
	Draft, Prerelease bool
	Assets            []githubAsset
}

func (s *Server) latestRelease(ctx context.Context, repo string) (githubRelease, error) {
	var release githubRelease
	if !repoName.MatchString(repo) {
		return release, errors.New("无效的 GitHub 仓库")
	}
	raw, err := s.github(ctx, "https://api.github.com/repos/"+repo+"/releases/latest", "application/vnd.github+json", 2<<20)
	if err != nil {
		return release, err
	}
	if json.Unmarshal(raw, &release) != nil || release.Draft || release.Prerelease || !strings.HasPrefix(release.Tag, "v") || !p.Version.MatchString(release.Tag) {
		return release, errors.New("GitHub 未返回有效的正式版本")
	}
	return release, nil
}

func (r githubRelease) asset(tool string) (githubAsset, error) {
	name := tool + "-" + r.Tag + ".json"
	for _, asset := range r.Assets {
		if asset.Name == name && asset.ID > 0 {
			return asset, nil
		}
	}
	return githubAsset{}, fmt.Errorf("发行版缺少 %s", name)
}

func (s *Server) releaseAsset(ctx context.Context, repo string, asset githubAsset, limit int64) ([]byte, error) {
	return s.githubDownload(ctx, "https://api.github.com/repos/"+repo+"/releases/assets/"+strconv.FormatInt(asset.ID, 10), "application/octet-stream", limit, true)
}

// Register the two artifacts of one published platform version atomically.
// Downloading and registering a release never dispatches an update.
func (s *Server) syncPlatformRelease(ctx context.Context) ([]p.Tool, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	const repo = "pypto-tools/pto-center"
	release, err := s.latestRelease(ctx, repo)
	if err != nil {
		return nil, err
	}
	var tools []p.Tool
	var files [][]byte
	for _, name := range []string{p.CenterTool, p.AgentTool} {
		asset, err := release.asset(name)
		if err != nil {
			return nil, err
		}
		raw, err := s.releaseAsset(ctx, repo, asset, p.MaxAgentBundle)
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
			return nil, fmt.Errorf("%s 的签名内容与组件或标签不符", asset.Name)
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

// Verification status is about an observed GitHub release. A successful local
// upload alone cannot prove that the repository's release workflow is working.
func (s *Server) checkPublisher(ctx context.Context, tool string, force bool) (PublisherInfo, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	k, err := readPublisher(s.Store.db.QueryRow("SELECT "+publisherCols+" FROM tool_publishers WHERE tool=?", tool))
	if err != nil {
		return k, err
	}
	err = s.checkPublisherRelease(ctx, k, force)
	if err != nil {
		if ctx.Err() != nil {
			return k, ctx.Err()
		}
		message := err.Error()
		var networkError *url.Error
		if errors.As(err, &networkError) {
			// Redirect URLs may contain temporary download credentials.
			message = "无法从 GitHub 下载发布包，请检查网络、代理或下载地址"
		}
		result, dbErr := s.Store.db.Exec("UPDATE tool_publishers SET state='failed',checked=?,message=? WHERE tool=? AND key=?", time.Now().Unix(), message, k.Tool, k.PublicKey)
		if dbErr != nil {
			return k, dbErr
		}
		if count, dbErr := result.RowsAffected(); dbErr != nil || count != 1 {
			return k, errPublisherChanged
		}
	}
	return readPublisher(s.Store.db.QueryRow("SELECT "+publisherCols+" FROM tool_publishers WHERE tool=? AND key=?", tool, k.PublicKey))
}

func (s *Server) checkPublisherRelease(ctx context.Context, k PublisherInfo, force bool) error {
	r, err := s.latestRelease(ctx, k.Repository)
	if err != nil {
		return err
	}
	a, err := r.asset(k.Tool)
	if err != nil {
		return err
	}
	if !force && k.Version == r.Tag && k.assetHash != "" && a.Digest == "sha256:"+k.assetHash {
		_, err = s.Store.db.Exec("UPDATE tool_publishers SET state='verified',checked=?,message='' WHERE tool=? AND key=?", time.Now().Unix(), k.Tool, k.PublicKey)
		return err
	}
	raw, err := s.releaseAsset(ctx, k.Repository, a, p.MaxBundle)
	if err != nil {
		return err
	}
	if a.Digest != "" && a.Digest != "sha256:"+p.Hash(raw) {
		return errors.New("发布附件与 GitHub 返回的摘要不一致")
	}
	_, err = s.Store.registerChecked(raw, nil, nil, &publisherCheck{k.ToolPublisher, r.Tag})
	if err == nil {
		s.wake()
	}
	return err
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
			publishers, _ := s.Store.publishers()
			for _, k := range publishers {
				if ctx.Err() != nil {
					return
				}
				_, _ = s.checkPublisher(ctx, k.Tool, false)
			}
		}
	}
}

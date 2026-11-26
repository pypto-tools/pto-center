package center

import (
	"context"
	"encoding/json"
	"errors"
	p "github.com/pypto-tools/pto-center/protocol/v1"
	"net/url"
	"time"
)

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

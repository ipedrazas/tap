package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// specs reaches the factory's /console/* routes, which the generic
// /api/factory/* proxy cannot (it only maps to /v1/*). The console checks
// that the caller may edit the agent before calling them; the factory's
// publisher then rechecks the edit itself and opens a pull request that
// changes only agents/<name>/agent.yaml.
type specs struct {
	base  *url.URL
	token func() string
	http  *http.Client
}

// AgentSpec is agent.yaml as it is on the base branch, and the commit the
// edit will be built on.
type AgentSpec struct {
	SHA       string `json:"sha"`
	AgentYAML string `json:"agentYaml"`
}

// EditRequest is what the console sends; Egress maps tool names to their
// complete new egress lists. Title nil leaves metadata.title alone.
type EditRequest struct {
	Base   string              `json:"base"`
	Title  *string             `json:"title,omitempty"`
	Egress map[string][]string `json:"egress,omitempty"`
}

type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

// statusError carries the factory's status so the console can pass 4xx on.
type statusError struct {
	Status int
	Msg    string
}

func (e *statusError) Error() string { return e.Msg }

func (s *specs) call(ctx context.Context, method, path, user string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	u := *s.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-tap-user", user)
	if s.token != nil {
		req.Header.Set("Authorization", "Bearer "+s.token())
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return &statusError{http.StatusBadGateway, "factory: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &statusError{resp.StatusCode, msg}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("factory %s: %w", path, err)
	}
	return nil
}

func (s *specs) get(ctx context.Context, agent, user string) (*AgentSpec, error) {
	var out AgentSpec
	if err := s.call(ctx, http.MethodGet, "/console/agents/"+url.PathEscape(agent), user, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *specs) edit(ctx context.Context, agent, user string, r EditRequest) (*PullRequest, error) {
	var out PullRequest
	if err := s.call(ctx, http.MethodPost, "/console/agents/"+url.PathEscape(agent)+"/pr", user, r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

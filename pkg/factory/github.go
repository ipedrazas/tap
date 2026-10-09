// Package factory is the publisher side of the factory: the only component
// that holds a GitHub token. It turns a finished job's files into a branch
// and a pull request through the GitHub REST API, and refuses anything
// outside the new agent's own directory. It never touches a filesystem the
// factory's tools can write.
package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitHub is a minimal REST client for one repository.
type GitHub struct {
	BaseURL string // https://api.github.com
	Repo    string // owner/name
	Token   string
	HTTP    *http.Client
}

// ErrNotFound is returned for 404 responses.
var ErrNotFound = errors.New("not found")

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Message) }

func (g *GitHub) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(g.BaseURL, "/")+"/repos/"+g.Repo+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &apiError{Status: resp.StatusCode, Message: e.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// BranchSHA returns the commit a branch points at.
func (g *GitHub) BranchSHA(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, "/git/ref/heads/"+url.PathEscape(branch), nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

// Exists reports whether path exists at ref.
func (g *GitHub) Exists(ctx context.Context, path, ref string) (bool, error) {
	err := g.do(ctx, http.MethodGet, "/contents/"+path+"?ref="+url.QueryEscape(ref), nil, &json.RawMessage{})
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (g *GitHub) commitTree(ctx context.Context, sha string) (string, error) {
	var c struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.do(ctx, http.MethodGet, "/git/commits/"+sha, nil, &c); err != nil {
		return "", err
	}
	return c.Tree.SHA, nil
}

func (g *GitHub) createBlob(ctx context.Context, content []byte) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	in := map[string]string{"content": b64std(content), "encoding": "base64"}
	if err := g.do(ctx, http.MethodPost, "/git/blobs", in, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func (g *GitHub) createTree(ctx context.Context, base string, entries []treeEntry) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	in := map[string]any{"base_tree": base, "tree": entries}
	if err := g.do(ctx, http.MethodPost, "/git/trees", in, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

func (g *GitHub) createCommit(ctx context.Context, msg, tree, parent string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	in := map[string]any{"message": msg, "tree": tree, "parents": []string{parent}}
	if err := g.do(ctx, http.MethodPost, "/git/commits", in, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

func (g *GitHub) createRef(ctx context.Context, branch, sha string) error {
	return g.do(ctx, http.MethodPost, "/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": sha}, nil)
}

// PullRequest is the part of GitHub's PR object we report.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

func (g *GitHub) createPR(ctx context.Context, title, body, head, base string) (PullRequest, error) {
	var pr PullRequest
	in := map[string]any{"title": title, "body": body, "head": head, "base": base}
	err := g.do(ctx, http.MethodPost, "/pulls", in, &pr)
	return pr, err
}

func (g *GitHub) addLabels(ctx context.Context, number int, labels ...string) error {
	return g.do(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/labels", number), map[string]any{"labels": labels}, nil)
}

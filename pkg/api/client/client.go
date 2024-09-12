package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gptscript-ai/go-gptscript"
	"github.com/gptscript-ai/otto/pkg/api"
	"github.com/gptscript-ai/otto/pkg/api/types"
	v1 "github.com/gptscript-ai/otto/pkg/storage/apis/otto.gptscript.ai/v1"
)

type Client struct {
	BaseURL string
	Token   string
}

func (c *Client) putJSON(ctx context.Context, path string, obj any, headerKV ...string) (*http.Request, *http.Response, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, err
	}
	return c.doRequest(ctx, http.MethodPut, path, bytes.NewBuffer(data), append(headerKV, "Content-Type", "application/json")...)
}

func (c *Client) postJSON(ctx context.Context, path string, obj any, headerKV ...string) (*http.Request, *http.Response, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, err
	}
	return c.doRequest(ctx, http.MethodPost, path, bytes.NewBuffer(data), append(headerKV, "Content-Type", "application/json")...)
}

func (c *Client) doStream(ctx context.Context, method, path string, body io.Reader, headerKV ...string) (*http.Request, *http.Response, error) {
	return c.doRequest(ctx, method, path, body, append(headerKV, "Accept", "text/event-stream")...)
}

func (c *Client) doRequest(ctx context.Context, method, path string, body io.Reader, headerKV ...string) (*http.Request, *http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if len(headerKV)%2 != 0 {
		return nil, nil, fmt.Errorf("length of headerKV must be even")
	}
	for i := 0; i < len(headerKV); i += 2 {
		req.Header.Add(headerKV[i], headerKV[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode > 399 {
		data, _ := io.ReadAll(resp.Body)
		msg := string(data)
		if len(msg) == 0 {
			msg = resp.Status
		}
		return nil, nil, &api.ErrHTTP{
			Code:    resp.StatusCode,
			Message: msg,
		}
	}
	return req, resp, err
}

func (c *Client) UpdateAgent(ctx context.Context, id string, manifest v1.AgentManifest) (*types.Agent, error) {
	_, resp, err := c.putJSON(ctx, fmt.Sprintf("/agents/%s", id), manifest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return toObject(resp, &types.Agent{})
}

func toStream[T any](resp *http.Response) chan T {
	ch := make(chan T)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		lines := bufio.NewScanner(resp.Body)
		for lines.Scan() {
			var obj T
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				if err := json.Unmarshal([]byte(data), &obj); err == nil {
					ch <- obj
				}
			}
		}
	}()
	return ch
}

func toObject[T any](resp *http.Response, obj T) (def T, _ error) {
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(obj); err != nil {
		return def, err
	}
	return obj, nil
}

func (c *Client) DeleteAgent(ctx context.Context, id string) error {
	_, resp, err := c.doRequest(ctx, http.MethodDelete, fmt.Sprintf("/agents/"+id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

type InvokeOptions struct {
	ThreadID string
}

func (c *Client) Invoke(ctx context.Context, agentID string, input string, opt ...InvokeOptions) (*types.InvokeResponse, error) {
	var (
		opts InvokeOptions
	)
	for _, o := range opt {
		if o.ThreadID != "" {
			opts.ThreadID = o.ThreadID
		}
	}

	url := fmt.Sprintf("/invoke/%s?events=true", agentID)
	if opts.ThreadID != "" {
		url = fmt.Sprintf("/invoke/%s/threads/%s?events=true", agentID, opts.ThreadID)
	}

	_, resp, err := c.doRequest(ctx, http.MethodPost, url, bytes.NewBuffer([]byte(input)), "Accept", "text/event-stream")
	if err != nil {
		return nil, err
	}

	events := make(chan types.Progress)
	go func() {
		defer resp.Body.Close()
		defer close(events)
		lines := bufio.NewScanner(resp.Body)
		for lines.Scan() {
			var event types.Progress
			data := strings.TrimPrefix(lines.Text(), "data: ")
			if len(data) == 0 {
				continue
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				events <- types.Progress{
					Error: err.Error(),
				}
			} else {
				events <- event
			}
		}

		if err := lines.Err(); err != nil {
			events <- types.Progress{
				Error: err.Error(),
			}
		}
	}()

	return &types.InvokeResponse{
		Events:   events,
		RunID:    resp.Header.Get("X-Otto-Run-Id"),
		ThreadID: resp.Header.Get("X-Otto-Thread-Id"),
	}, nil
}

func (c *Client) GetAgent(ctx context.Context, id string) (*types.Agent, error) {
	_, resp, err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("/agents/"+id), nil)
	if err != nil {
		return nil, err
	}

	return toObject(resp, &types.Agent{})
}

func (c *Client) CreateAgent(ctx context.Context, agent v1.AgentManifest) (*types.Agent, error) {
	_, resp, err := c.postJSON(ctx, fmt.Sprintf("/agents"), agent)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return toObject(resp, &types.Agent{})
}

type ListAgentsOptions struct {
	Slug string
}

func (c *Client) ListAgents(ctx context.Context, opts ...ListAgentsOptions) (result types.AgentList, err error) {
	defer func() {
		sort.Slice(result.Items, func(i, j int) bool {
			return result.Items[i].Metadata.Created.Before(result.Items[j].Metadata.Created)
		})
	}()

	var opt ListAgentsOptions
	for _, o := range opts {
		if o.Slug != "" {
			opt.Slug = o.Slug
		}
	}

	_, resp, err := c.doRequest(ctx, http.MethodGet, "/agents", nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	_, err = toObject(resp, &result)
	if err != nil {
		return result, err
	}

	if opt.Slug != "" {
		var filtered types.AgentList
		for _, agent := range result.Items {
			if agent.Slug == opt.Slug && agent.SlugAssigned {
				filtered.Items = append(filtered.Items, agent)
			}
		}
		result = filtered
	}

	return
}

func (c *Client) DeleteThread(ctx context.Context, id string) error {
	_, resp, err := c.doRequest(ctx, http.MethodDelete, fmt.Sprintf("/threads/"+id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

type ListThreadsOptions struct {
	AgentID string
}

func (c *Client) ListThreads(ctx context.Context, opts ...ListThreadsOptions) (result types.ThreadList, err error) {
	defer func() {
		sort.Slice(result.Items, func(i, j int) bool {
			return result.Items[i].Created.Before(result.Items[j].Created)
		})
	}()

	var opt ListThreadsOptions
	for _, o := range opts {
		if o.AgentID != "" {
			opt.AgentID = o.AgentID
		}
	}
	url := "/threads"
	if opt.AgentID != "" {
		url = fmt.Sprintf("/agents/%s", opt.AgentID) + url
	}
	_, resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	_, err = toObject(resp, &result)
	return
}

type ListRunsOptions struct {
	AgentID  string
	ThreadID string
}

func (c *Client) DebugRun(ctx context.Context, runID string) (result types.RunDebug, err error) {
	_, resp, err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("/runs/%s/debug", runID), nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	result.Frames = map[string]gptscript.CallFrame{}
	err = json.NewDecoder(resp.Body).Decode(&result.Frames)
	return
}

func (c *Client) StreamRuns(ctx context.Context, opts ...ListRunsOptions) (result <-chan types.Run, err error) {
	url := c.runURLFromOpts(opts...)
	_, resp, err := c.doStream(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}

	return toStream[types.Run](resp), nil
}

func (c *Client) runURLFromOpts(opts ...ListRunsOptions) string {
	var opt ListRunsOptions
	for _, o := range opts {
		if o.ThreadID != "" {
			opt.ThreadID = o.ThreadID
		}
		if o.AgentID != "" {
			opt.AgentID = o.AgentID
		}
	}
	url := "/runs"
	if opt.AgentID != "" && opt.ThreadID != "" {
		url = fmt.Sprintf("/agents/%s/threads/%s/runs", opt.AgentID, opt.ThreadID)
	} else if opt.AgentID != "" {
		url = fmt.Sprintf("/agents/%s/runs", opt.AgentID)
	} else if opt.ThreadID != "" {
		url = fmt.Sprintf("/threads/%s/runs", opt.ThreadID)
	}
	return url
}

func (c *Client) ListRuns(ctx context.Context, opts ...ListRunsOptions) (result types.RunList, err error) {
	defer func() {
		sort.Slice(result.Items, func(i, j int) bool {
			return result.Items[i].Created.Before(result.Items[j].Created)
		})
	}()

	url := c.runURLFromOpts(opts...)
	_, resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	_, err = toObject(resp, &result)
	return
}

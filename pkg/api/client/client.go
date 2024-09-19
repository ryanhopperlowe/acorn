package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gptscript-ai/otto/pkg/api"
	"github.com/gptscript-ai/otto/pkg/mvl"
	"k8s.io/utils/strings/slices"
)

var log = mvl.Package()

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
	if log.IsDebug() {
		var (
			data    = "[NONE]"
			headers string
		)
		if body != nil {
			dataBytes, err := io.ReadAll(body)
			if err != nil {
				return nil, nil, err
			}
			if utf8.Valid(dataBytes) {
				data = string(dataBytes)
			} else {
				data = fmt.Sprintf("[BINARY DATA len(%d)]", len(dataBytes))
			}

			body = bytes.NewReader(dataBytes)
		}
		// Convert headerKV... into a string of format k1=v1, k2=v2, ...
		for i := 0; i < len(headerKV); i += 2 {
			headers += fmt.Sprintf("%s=%s, ", headerKV[i], headerKV[i+1])
		}
		log.Fields("method", method, "path", path, "body", data, "headers", headers).Debugf("HTTP Request")
	}

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
	if log.IsDebug() && !slices.Contains(headerKV, "text/event-stream") {
		var data string
		dataBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		if utf8.Valid(dataBytes) {
			data = string(dataBytes)
		} else {
			data = fmt.Sprintf("[BINARY DATA len(%d)]", len(dataBytes))
		}
		log.Fields("method", method, "path", path, "body", data, "code", resp.StatusCode).Debugf("HTTP Response")
		resp.Body = io.NopCloser(bytes.NewReader(dataBytes))
	}
	return req, resp, err
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
				if log.IsDebug() {
					log.Fields("data", data).Debugf("Received data")
				}
				if err := json.Unmarshal([]byte(data), &obj); err == nil {
					ch <- obj
				} else {
					errBytes, _ := json.Marshal(map[string]any{
						"error": err.Error(),
					})
					if err := json.Unmarshal(errBytes, &obj); err == nil {
						ch <- obj
					}
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

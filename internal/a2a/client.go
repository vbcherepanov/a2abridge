package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is an A2A JSON-RPC 2.0 client.
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:49152 or https://...
	HTTP    *http.Client
}

// DefaultTransport is consulted by NewClient when callers don't supply
// their own *http.Client. Bridges set it to a TLS-aware transport when
// running with mTLS so every a2a.NewClient() call inherits the right
// certs without each call site re-plumbing tls.Config.
var DefaultTransport http.RoundTripper

func NewClient(baseURL string) *Client {
	c := &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 0}, // streaming needs no default timeout
	}
	if DefaultTransport != nil {
		c.HTTP.Transport = DefaultTransport
	}
	return c
}

// FetchAgentCard GETs /.well-known/a2a.
func (c *Client) FetchAgentCard(ctx context.Context) (*AgentCard, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+WellKnownPath, nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("agent card: %s", resp.Status)
	}
	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, err
	}
	return &card, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id, _ := json.Marshal(time.Now().UnixNano())
	rawParams, _ := json.Marshal(params)
	body, _ := json.Marshal(JSONRPCRequest{
		JSONRPC: "2.0", ID: id, Method: method, Params: rawParams,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", ProtocolVersion)

	httpClient := &http.Client{Timeout: 60 * time.Second}
	if DefaultTransport != nil {
		httpClient.Transport = DefaultTransport
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r struct {
		JSONRPCResponse
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("rpc %s: %d %s", method, r.Error.Code, r.Error.Message)
	}
	if out != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// SendMessage — a2a.SendMessage. Result is Task-or-Message union.
type SendMessageResult struct {
	Task    *Task
	Message *Message
}

func (c *Client) SendMessage(ctx context.Context, p MessageSendParams) (*SendMessageResult, error) {
	var raw json.RawMessage
	if err := c.call(ctx, MethodSendMessage, p, &raw); err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(raw, &t); err == nil && t.ID != "" {
		return &SendMessageResult{Task: &t}, nil
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err == nil && m.MessageID != "" {
		return &SendMessageResult{Message: &m}, nil
	}
	return nil, errors.New("SendMessage: unknown result shape")
}

func (c *Client) GetTask(ctx context.Context, id string) (*Task, error) {
	var t Task
	if err := c.call(ctx, MethodGetTask, TaskIDParams{ID: id}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *Client) CancelTask(ctx context.Context, id string) (*Task, error) {
	var t Task
	if err := c.call(ctx, MethodCancelTask, TaskIDParams{ID: id}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// SendStreamingMessage opens an SSE stream and emits events until terminal state or ctx done.
func (c *Client) SendStreamingMessage(ctx context.Context, p MessageSendParams, out chan<- StreamResponse) error {
	return c.openStream(ctx, MethodSendStreamingMessage, p, out)
}

func (c *Client) SubscribeToTask(ctx context.Context, id string, out chan<- StreamResponse) error {
	return c.openStream(ctx, MethodSubscribeToTask, TaskIDParams{ID: id}, out)
}

func (c *Client) openStream(ctx context.Context, method string, params any, out chan<- StreamResponse) error {
	rpcID, _ := json.Marshal(time.Now().UnixNano())
	rawParams, _ := json.Marshal(params)
	body, _ := json.Marshal(JSONRPCRequest{JSONRPC: "2.0", ID: rpcID, Method: method, Params: rawParams})

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("A2A-Version", ProtocolVersion)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stream: %s %s", resp.Status, b)
	}

	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var env struct {
			Result StreamResponse `json:"result"`
			Error  *JSONRPCError  `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			continue
		}
		if env.Error != nil {
			return fmt.Errorf("stream rpc: %d %s", env.Error.Code, env.Error.Message)
		}
		select {
		case out <- env.Result:
		case <-ctx.Done():
			return ctx.Err()
		}
		// terminate if statusUpdate.final == true
		if env.Result.StatusUpdate != nil && env.Result.StatusUpdate.Final {
			return nil
		}
	}
}

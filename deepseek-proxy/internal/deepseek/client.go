package deepseek

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

// DefaultBaseURL is the web app origin.
const DefaultBaseURL = "https://chat.deepseek.com"

// DefaultUserAgent mirrors a desktop Chrome session.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// ErrUnauthorized means the web token expired or was never valid.
var ErrUnauthorized = errors.New("deepseek web token rejected; run deepseek-openai -login")

// ErrRateLimited means the account is throttled.
var ErrRateLimited = errors.New("deepseek web rate limit")

// StatusError carries a non-200 web response without the prompt.
type StatusError struct {
	Status int
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("deepseek HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("deepseek HTTP %d", e.Status)
}

// Client talks to the DeepSeek web API.
type Client struct {
	store *Store
	http  *http.Client
	base  string
}

// NewClient builds a client with no overall timeout: completions stream for
// minutes and are bounded by the request context instead.
func NewClient(store *Store, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		store: store,
		base:  strings.TrimRight(baseURL, "/"),
		http:  &http.Client{Timeout: 0},
	}
}

// HasToken reports whether the store holds a token, without exposing it.
func (c *Client) HasToken() bool { return c.store.Token() != "" }

func (c *Client) headers(pow string) http.Header {
	data := c.store.Data()
	h := make(http.Header)
	h.Set("User-Agent", DefaultUserAgent)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Origin", c.base)
	h.Set("Referer", c.base+"/")
	h.Set("x-client-platform", "web")
	h.Set("x-client-version", "2.0.0")
	h.Set("x-client-locale", "en")
	h.Set("x-client-timezone-offset", "0")
	h.Set("x-app-version", "2.0.0")
	h.Set("Content-Type", "application/json")
	if data.Token != "" {
		h.Set("Authorization", "Bearer "+data.Token)
	}
	if data.Cookie != "" {
		h.Set("Cookie", data.Cookie)
	}
	if data.HifDliq != "" {
		h.Set("x-hif-dliq", data.HifDliq)
	}
	if data.HifLeim != "" {
		h.Set("x-hif-leim", data.HifLeim)
	}
	if pow != "" {
		h.Set("x-ds-pow-response", pow)
	}
	return h
}

func (c *Client) post(ctx context.Context, path, pow string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(pow)
	return c.do(req)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		return nil, &StatusError{Status: resp.StatusCode, Body: truncate(body, 200)}
	}
	return body, nil
}

// APIError is a failure envelope returned with HTTP 200.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("deepseek API failed (%d): %s", e.Code, e.Msg)
}

// errForAPICode maps a web API failure onto a sentinel when the meaning is
// stable, so an expired token is not reported as a transport error.
func errForAPICode(code int, msg string) error {
	if code == 40003 {
		return ErrUnauthorized
	}
	return &APIError{Code: code, Msg: msg}
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// CreateSession returns a fresh remote chat session id.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	body, err := c.post(ctx, "/api/v0/chat_session/create", "", struct{}{})
	if err != nil {
		return "", err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			BizData struct {
				ID          string `json:"id"`
				ChatSession struct {
					ID string `json:"id"`
				} `json:"chat_session"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("unexpected chat_session/create response: %w", err)
	}
	if res.Code != 0 {
		return "", fmt.Errorf("chat_session/create: %w", errForAPICode(res.Code, res.Msg))
	}
	id := res.Data.BizData.ChatSession.ID
	if id == "" {
		id = res.Data.BizData.ID
	}
	if id == "" {
		return "", errors.New("chat_session/create returned no session id")
	}
	return id, nil
}

// powFor requests and solves the proof of work for a target path.
func (c *Client) powFor(ctx context.Context, targetPath string) (string, error) {
	body, err := c.post(ctx, "/api/v0/chat/create_pow_challenge", "", map[string]string{"target_path": targetPath})
	if err != nil {
		return "", err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			BizData map[string]any `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("unexpected pow response: %w", err)
	}
	if res.Code != 0 {
		return "", fmt.Errorf("pow challenge: %w", errForAPICode(res.Code, res.Msg))
	}
	target := res.Data.BizData
	if sub, ok := target["challenge"].(map[string]any); ok {
		target = sub
	}
	chal := PowChallenge{
		Algorithm:  str(target, "algorithm", "DeepSeekHashV1"),
		Challenge:  str(target, "challenge", ""),
		Salt:       str(target, "salt", ""),
		Difficulty: num(target, "difficulty", 144000),
		ExpireAt:   num(target, "expire_at", 0),
		Signature:  str(target, "signature", ""),
		TargetPath: str(target, "target_path", targetPath),
	}
	if chal.Algorithm == "" {
		chal.Algorithm = "DeepSeekHashV1"
	}
	if chal.TargetPath == "" {
		chal.TargetPath = targetPath
	}
	return SolvePoW(chal)
}

func str(m map[string]any, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

func num(m map[string]any, key string, def int64) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return def
}

// CompleteRequest is one turn against a remote session.
type CompleteRequest struct {
	Prompt    string
	SessionID string
	Parent    string
	Model     ModelConfig
	Thinking  bool
	Search    bool
}

// Complete streams one turn. The caller must drain both channels; the chunk
// channel closes first, then the error channel closes.
func (c *Client) Complete(ctx context.Context, req CompleteRequest) (<-chan ChatChunk, <-chan error) {
	chunks := make(chan ChatChunk, 32)
	errs := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer close(errs)
		if err := c.complete(ctx, req, chunks); err != nil {
			errs <- err
		}
	}()
	return chunks, errs
}

func (c *Client) complete(ctx context.Context, req CompleteRequest, chunks chan<- ChatChunk) error {
	pow, err := c.powFor(ctx, "/api/v0/chat/completion")
	if err != nil {
		return err
	}
	var parent any
	if req.Parent != "" {
		parent = req.Parent
	}
	payload := map[string]any{
		"chat_session_id":   req.SessionID,
		"parent_message_id": parent,
		"model_type":        req.Model.ModelType,
		"prompt":            req.Prompt,
		"ref_file_ids":      []string{},
		"thinking_enabled":  req.Thinking,
		"search_enabled":    req.Search,
		"action":            nil,
		"preempt":           false,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v0/chat/completion", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header = c.headers(pow)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Status: resp.StatusCode, Body: truncate(b, 200)}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	parser := NewStreamParser()
	for scanner.Scan() {
		chunk, err := parser.Feed(scanner.Text())
		if err != nil {
			return err
		}
		if chunk == nil {
			continue
		}
		chunk.SessionID = req.SessionID
		select {
		case chunks <- *chunk:
		case <-ctx.Done():
			return ctx.Err()
		}
		if chunk.FinishReason != "" {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stream read failed: %w", err)
	}
	return nil
}

// HistoryMessage is one message read back from a remote session.
type HistoryMessage struct {
	ID      string
	Role    string
	Content string
}

// History reads the remote session transcript. It is best-effort: the endpoint
// is not part of any public contract and may answer with an error shape.
func (c *Client) History(ctx context.Context, sessionID string) ([]HistoryMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	body, err := c.post(ctx, "/api/v0/chat/history_messages", "", map[string]string{"chat_session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			BizData struct {
				ChatMessages []struct {
					MessageID string `json:"message_id"`
					Role      string `json:"role"`
					Content   string `json:"content"`
				} `json:"chat_messages"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("unexpected history response: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("history: %w", errForAPICode(res.Code, res.Msg))
	}
	out := make([]HistoryMessage, 0, len(res.Data.BizData.ChatMessages))
	for _, m := range res.Data.BizData.ChatMessages {
		out = append(out, HistoryMessage{ID: m.MessageID, Role: m.Role, Content: m.Content})
	}
	return out, nil
}

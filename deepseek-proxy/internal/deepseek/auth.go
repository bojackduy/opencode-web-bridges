package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AuthData is the browser session used for the web API.
type AuthData struct {
	Token   string `json:"token"`
	Cookie  string `json:"cookie,omitempty"`
	HifDliq string `json:"hif_dliq,omitempty"`
	HifLeim string `json:"hif_leim,omitempty"`
}

// Store reads and writes the auth file. Environment variables win over the
// file so a one-off run never needs to touch disk.
type Store struct {
	path string
	data AuthData
}

// NewStore creates a store at path (empty means ~/.deepseek-proxy/auth.json).
func NewStore(path string) (*Store, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".deepseek-proxy", "auth.json")
	}
	s := &Store{path: path}
	if tok := CleanToken(os.Getenv("DEEPSEEK_TOKEN")); tok != "" {
		s.data = AuthData{
			Token:   tok,
			Cookie:  strings.TrimSpace(os.Getenv("DEEPSEEK_COOKIE")),
			HifDliq: strings.TrimSpace(os.Getenv("DEEPSEEK_HIF_DLIQ")),
			HifLeim: strings.TrimSpace(os.Getenv("DEEPSEEK_HIF_LEIM")),
		}
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var data AuthData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("auth file %s is not valid JSON: %w", path, err)
	}
	data.Token = CleanToken(data.Token)
	s.data = data
	return s, nil
}

// Path returns the auth file location.
func (s *Store) Path() string { return s.path }

// Data returns a copy of the current credentials.
func (s *Store) Data() AuthData { return s.data }

// Token returns the bearer token, or "" when not configured.
func (s *Store) Token() string { return s.data.Token }

// Set stores credentials in memory and, when persist is true, on disk.
func (s *Store) Set(data AuthData, persist bool) error {
	data.Token = CleanToken(data.Token)
	if data.Token == "" {
		return errors.New("token is empty")
	}
	s.data = data
	if !persist {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// CleanToken accepts a pasted token in any of the shapes people copy: bare
// value, "Bearer ...", quoted JSON string, or a "userToken" line.
func CleanToken(raw string) string {
	t := strings.TrimSpace(raw)
	t = strings.Trim(t, `"'`)
	if i := strings.LastIndex(t, "="); i >= 0 && strings.Contains(t[:i], "userToken") {
		t = t[i+1:]
	}
	t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "Bearer "))
	t = strings.Trim(t, `"'`)
	return t
}

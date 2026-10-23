package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// StoredTokens is the persisted credential set. RefreshToken is the one
// long-lived secret: it rotates on every OAuth refresh (old one invalidated),
// so it MUST be written back before any refreshed token is handed out.
// AccessToken is a cache; AccessExpiresAt is epoch milliseconds, 0 for none.
type StoredTokens struct {
	RefreshToken    string `json:"refresh_token"`
	AccessToken     string `json:"access_token,omitempty"`
	AccessExpiresAt int64  `json:"access_expires_at,omitempty"`
}

type TokenStore interface {
	// The stored tokens, or nil when no one has logged in yet.
	Read() (*StoredTokens, error)
	// Persist tokens durably. Implementations must be crash-atomic.
	Write(tokens StoredTokens) error
}

// FileTokenStore is the file-on-volume store. It mirrors the proven chandlery
// `tools/hytale-token` semantics: the credential file (and its directory) are
// owner-only, and writes are atomic (a temp file chmod 600 then renamed over
// the target) so a crash after a refresh can never strand a spent refresh
// token as the stored one or briefly expose the file world-readable.
//
// There is no OS-level lock here (unlike the multi-process CLI it descends
// from): Treesinger is the sole, single-process writer, and refresh is
// serialised in memory (see OAuthClient's single-flight). Running multiple
// writer instances against one store would reintroduce the race and is out of
// scope by design.
type FileTokenStore struct {
	path string
}

func NewFileTokenStore(path string) *FileTokenStore { return &FileTokenStore{path: path} }

func (s *FileTokenStore) Read() (*StoredTokens, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tokens StoredTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return &tokens, nil
}

func (s *FileTokenStore) Write(tokens StoredTokens) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", s.path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// WriteFile honours the umask, and leaves an existing file's mode alone;
	// force owner-only.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// MemoryTokenStore is an in-memory store for tests. Same contract, no disk.
type MemoryTokenStore struct {
	mu     sync.Mutex
	tokens *StoredTokens
}

func NewMemoryTokenStore(seed *StoredTokens) *MemoryTokenStore {
	return &MemoryTokenStore{tokens: copyOf(seed)}
}

func (s *MemoryTokenStore) Read() (*StoredTokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyOf(s.tokens), nil
}

func (s *MemoryTokenStore) Write(tokens StoredTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = &tokens
	return nil
}

// copyOf is a copy of tokens, so a caller never holds the stored value.
func copyOf(tokens *StoredTokens) *StoredTokens {
	if tokens == nil {
		return nil
	}
	copied := *tokens
	return &copied
}

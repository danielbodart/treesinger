package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// StoredTokens is the persisted credential set. RefreshToken is the one
// long-lived secret: it rotates on every OAuth refresh (old one invalidated),
// so it MUST be written back before any refreshed token is handed out.
// AccessToken is a cache; AccessExpiresAt is epoch milliseconds.
type StoredTokens struct {
	RefreshToken    string `json:"refresh_token"`
	AccessToken     string `json:"access_token,omitempty"`
	AccessExpiresAt *int64 `json:"access_expires_at,omitempty"`
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
	tokens, err := decodeStoredTokens(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return tokens, nil
}

// decodeStoredTokens reads the token file as the TypeScript's JSON.parse does,
// since the two share the file across the migration. Only a file that is not
// JSON is an error; a field of an unexpected type is read the way JavaScript
// would use it, so a fractional, exponent or string `access_expires_at` is
// still a time, and an unusable one leaves the token to be refreshed rather
// than failing every call. A document that is false-y (null, 0, "") is no
// login; any other non-object (an array) is a login with no refresh token.
func decodeStoredTokens(data []byte) (*StoredTokens, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	if isFalsy(data) {
		return nil, nil
	}
	object, _ := jsonObject(data)
	tokens := &StoredTokens{
		RefreshToken: jsonText(object["refresh_token"]),
		AccessToken:  jsonText(object["access_token"]),
	}
	if present(object["access_expires_at"]) {
		tokens.AccessExpiresAt = millisAt(jsonNumber(object["access_expires_at"]))
	}
	return tokens, nil
}

// millisAt is an epoch-millisecond expiry as stored, from a JavaScript
// number: rounded up, which compares against a whole-millisecond clock exactly
// as the fraction would, and none at all for NaN, which compares false (stale)
// with everything.
func millisAt(ms float64) *int64 {
	if math.IsNaN(ms) {
		return nil
	}
	// Clamped well inside int64, so subtracting the skew cannot wrap.
	const limit = 1 << 62
	at := int64(math.Max(-limit, math.Min(limit, math.Ceil(ms))))
	return &at
}

func (s *FileTokenStore) Write(tokens StoredTokens) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := marshal(tokens)
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
	return &MemoryTokenStore{tokens: seed.copy()}
}

func (s *MemoryTokenStore) Read() (*StoredTokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens.copy(), nil
}

func (s *MemoryTokenStore) Write(tokens StoredTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = tokens.copy()
	return nil
}

func (t *StoredTokens) copy() *StoredTokens {
	if t == nil {
		return nil
	}
	copied := *t
	if t.AccessExpiresAt != nil {
		at := *t.AccessExpiresAt
		copied.AccessExpiresAt = &at
	}
	return &copied
}

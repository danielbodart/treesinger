package broker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFileTokenStore(t *testing.T) {
	storeAt := func(t *testing.T) (*FileTokenStore, string) {
		dir := filepath.Join(t.TempDir(), "nested")
		return NewFileTokenStore(filepath.Join(dir, "tokens.json")), dir
	}

	t.Run("read is nil before any write", func(t *testing.T) {
		store, _ := storeAt(t)
		tokens, err := store.Read()
		if tokens != nil || err != nil {
			t.Errorf("got %v, %v", tokens, err)
		}
	})

	t.Run("round-trips tokens, owner-only, with no temp file left behind", func(t *testing.T) {
		store, dir := storeAt(t)
		written := StoredTokens{RefreshToken: "rt", AccessToken: "at", AccessExpiresAt: 123}
		if err := store.Write(written); err != nil {
			t.Fatal(err)
		}

		read, _ := store.Read()
		if !reflect.DeepEqual(*read, written) {
			t.Errorf("got %+v, want %+v", *read, written)
		}

		data, _ := os.ReadFile(filepath.Join(dir, "tokens.json"))
		assertEqual(t, string(data), `{"refresh_token":"rt","access_token":"at","access_expires_at":123}`)
		file, _ := os.Stat(filepath.Join(dir, "tokens.json"))
		assertEqual(t, file.Mode().Perm(), 0o600)
		directory, _ := os.Stat(dir)
		assertEqual(t, directory.Mode().Perm(), 0o700)
		entries, _ := os.ReadDir(dir)
		assertEqual(t, len(entries), 1) // atomic rename left no *.tmp
		assertEqual(t, entries[0].Name(), "tokens.json")
	})

	t.Run("a second write replaces the first", func(t *testing.T) {
		store, _ := storeAt(t)
		store.Write(StoredTokens{RefreshToken: "rt1"})
		store.Write(StoredTokens{RefreshToken: "rt2"})
		read, _ := store.Read()
		assertEqual(t, read.RefreshToken, "rt2")
	})
}

func TestMemoryTokenStore(t *testing.T) {
	t.Run("round-trips and returns copies, not the held reference", func(t *testing.T) {
		store := NewMemoryTokenStore(nil)
		written := &StoredTokens{RefreshToken: "rt"}
		store.Write(*written)
		read, _ := store.Read()
		assertEqual(t, *read, *written)
		if read == written {
			t.Error("read returned the written reference")
		}
	})
}

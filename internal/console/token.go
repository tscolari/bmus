package console

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// TokenFile is the name of the per-start token file written under a
// bridge home.
const TokenFile = "console.token"

// generateToken returns a fresh 256-bit, hex-encoded (64 character) token.
func generateToken() (string, error) {
	return randomHex(32)
}

// writeTokenFile writes token to a 0600 temp file in home and renames it
// to <home>/TokenFile, so a reader never sees a partial token.
func writeTokenFile(home, token string) error {
	f, err := os.CreateTemp(home, TokenFile+".*")
	if err != nil {
		return fmt.Errorf("write console token: %w", err)
	}
	_, err = f.WriteString(token + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(home, TokenFile))
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("write console token: %w", err)
	}
	return nil
}

// RemoveTokenFile removes <home>/TokenFile and any temp file a crashed
// writeTokenFile left behind.
func RemoveTokenFile(home string) error {
	temps, _ := filepath.Glob(filepath.Join(home, TokenFile+".*"))
	for _, p := range append(temps, filepath.Join(home, TokenFile)) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove console token: %w", err)
		}
	}
	return nil
}

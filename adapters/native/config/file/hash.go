package file

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// fileHash supplies the legacy Watch baseline when no Load hash was provided.
func fileHash(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, fmt.Errorf("file config watcher: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, fmt.Errorf("file config watcher: hash %q: %w", path, err)
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

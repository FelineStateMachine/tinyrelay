package gitrelay

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
)

// StorageInventory reports retained bytes for one accepted repository. It is
// deliberately derived from the local object store so the self-hosted result
// has no hosted fuel or quota fields.
func (g *GitRelay) StorageInventory(ctx context.Context, owner, identifier string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repo, err := g.lookup(owner, identifier)
	if err != nil {
		return nil, errors.New("not found: repository")
	}
	var files, bytes int64
	err = filepath.WalkDir(g.repoPath(repo), func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"repository": map[string]any{"owner": owner, "identifier": identifier, "announcement": repo.EventID},
		"backend":    "filesystem",
		"objects":    map[string]any{"count": files, "rawBytes": bytes, "compressedBytes": bytes, "metadataBytes": int64(0)},
		"refs":       len(repo.Refs),
		"receipts":   0,
	}, nil
}

package remarkable

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sync/singleflight"
)

var materializations singleflight.Group

type contentCache struct {
	dir    string
	client Client
}

func newContentCache(dir string, client Client) *contentCache {
	return &contentCache{dir: dir, client: client}
}

func (c *contentCache) path(id string, version int) (string, error) {
	if id == "" || id == "." || filepath.Base(id) != id {
		return "", fmt.Errorf("invalid document UUID %q", id)
	}
	return filepath.Join(c.dir, id, strconv.Itoa(version)+".rmdoc"), nil
}

func (c *contentCache) materialize(ctx context.Context, item Item) (string, os.FileInfo, error) {
	cachePath, err := c.path(item.ID, item.Version)
	if err != nil {
		return "", nil, err
	}

	value, err, _ := materializations.Do(cachePath, func() (any, error) {
		if info, err := os.Stat(cachePath); err == nil {
			return info, nil
		} else if !os.IsNotExist(err) {
			return nil, err
		}

		dir := filepath.Dir(cachePath)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(dir, ".materializing-*")
		if err != nil {
			return nil, err
		}
		tmpPath := tmp.Name()
		committed := false
		defer func() {
			_ = tmp.Close()
			if !committed {
				_ = os.Remove(tmpPath)
			}
		}()

		if err := c.client.Download(ctx, item.ID, tmp); err != nil {
			return nil, fmt.Errorf("download document %q: %w", item.ID, err)
		}
		if err := tmp.Sync(); err != nil {
			return nil, err
		}
		if err := tmp.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(tmpPath, cachePath); err != nil {
			return nil, err
		}
		committed = true
		return os.Stat(cachePath)
	})
	if err != nil {
		return "", nil, err
	}
	return cachePath, value.(os.FileInfo), nil
}

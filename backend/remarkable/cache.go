package remarkable

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	rmarchive "github.com/juruen/rmapi/archive"
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

func (c *contentCache) promoteMetadata(oldItem, newItem Item) (os.FileInfo, bool, error) {
	oldPath, err := c.path(oldItem.ID, oldItem.Version)
	if err != nil {
		return nil, false, err
	}
	newPath, err := c.path(newItem.ID, newItem.Version)
	if err != nil {
		return nil, false, err
	}
	if info, err := os.Stat(newPath); err == nil {
		return info, true, nil
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}

	value, err, _ := materializations.Do(newPath, func() (any, error) {
		return rewriteRMDOCMetadata(oldPath, newPath, newItem)
	})
	if err != nil {
		return nil, false, err
	}
	return value.(os.FileInfo), true, nil
}

func rewriteRMDOCMetadata(oldPath, newPath string, item Item) (os.FileInfo, error) {
	source, err := zip.OpenReader(oldPath)
	if err != nil {
		return nil, err
	}
	defer source.Close()

	tmp, err := os.CreateTemp(filepath.Dir(newPath), ".promoting-*")
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

	writer := zip.NewWriter(tmp)
	for _, entry := range source.File {
		header := entry.FileHeader
		dst, err := writer.CreateHeader(&header)
		if err != nil {
			return nil, err
		}
		src, err := entry.Open()
		if err != nil {
			return nil, err
		}
		if entry.Name == item.ID+".metadata" {
			var metadata rmarchive.MetadataFile
			if err := json.NewDecoder(src).Decode(&metadata); err != nil {
				_ = src.Close()
				return nil, err
			}
			metadata.DocName = item.Name
			metadata.Parent = item.ParentID
			metadata.Version = item.Version
			if !item.ModTime.IsZero() {
				metadata.LastModified = strconv.FormatInt(item.ModTime.UnixMilli(), 10)
			}
			err = json.NewEncoder(dst).Encode(metadata)
		} else {
			_, err = io.Copy(dst, src)
		}
		closeErr := src.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, newPath); err != nil {
		return nil, err
	}
	committed = true
	return os.Stat(newPath)
}

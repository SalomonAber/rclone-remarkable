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
	"sync"

	rmarchive "github.com/juruen/rmapi/archive"
	"github.com/rclone/rclone/fs"
	"golang.org/x/sync/singleflight"
)

var materializations singleflight.Group

type contentCache struct {
	dir    string
	client Client

	// Both are keyed by archive cache path, so a new document version is
	// examined afresh.
	mu       sync.Mutex
	payloads map[string]payloadInfo
	hashes   map[string]string
}

func newContentCache(dir string, client Client) *contentCache {
	return &contentCache{dir: dir, client: client, payloads: make(map[string]payloadInfo), hashes: make(map[string]string)}
}

// String identifies the cache in log messages; it has no Fs to borrow a name from.
func (c *contentCache) String() string { return "reMarkable content cache" }

func (c *contentCache) path(item Item) (string, error) {
	if item.ID == "" || item.ID == "." || filepath.Base(item.ID) != item.ID {
		return "", fmt.Errorf("invalid document UUID %q", item.ID)
	}
	name := strconv.Itoa(item.Version)
	if !item.ModTime.IsZero() {
		name += "-" + strconv.FormatInt(item.ModTime.UnixNano(), 10)
	}
	return filepath.Join(c.dir, item.ID, name+".rmdoc"), nil
}

func (c *contentCache) materialize(ctx context.Context, item Item) (string, os.FileInfo, error) {
	cachePath, err := c.path(item)
	if err != nil {
		return "", nil, err
	}

	value, err, _ := materializations.Do(cachePath, func() (any, error) {
		if info, err := os.Stat(cachePath); err == nil {
			fs.Debugf(c, "Document %q version %d is already cached (%d bytes)", item.ID, item.Version, info.Size())
			return info, nil
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		fs.Debugf(c, "Materializing document %q version %d into %q", item.ID, item.Version, cachePath)

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
		info, err := os.Stat(cachePath)
		if err == nil {
			fs.Debugf(c, "Materialized document %q version %d (%d bytes)", item.ID, item.Version, info.Size())
		}
		return info, err
	})
	if err != nil {
		return "", nil, err
	}
	return cachePath, value.(os.FileInfo), nil
}

func (c *contentCache) promoteMetadata(oldItem, newItem Item) (os.FileInfo, bool, error) {
	return c.promote(oldItem, newItem, "", "")
}

// promote derives the cached archive of newItem from that of oldItem, so a
// metadata change or a payload this process uploaded itself costs no
// download. When payloadName is set, that entry is replaced by payloadPath.
func (c *contentCache) promote(oldItem, newItem Item, payloadName, payloadPath string) (os.FileInfo, bool, error) {
	oldPath, err := c.path(oldItem)
	if err != nil {
		return nil, false, err
	}
	newPath, err := c.path(newItem)
	if err != nil {
		return nil, false, err
	}
	if info, err := os.Stat(newPath); err == nil {
		fs.Debugf(c, "Document %q is already cached under its new metadata", newItem.ID)
		return info, true, nil
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		fs.Debugf(c, "Document %q has no cached archive to promote", newItem.ID)
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}

	value, err, _ := materializations.Do(newPath, func() (any, error) {
		return rewriteRMDOCMetadata(oldPath, newPath, newItem, payloadName, payloadPath)
	})
	if err != nil {
		return nil, false, err
	}
	fs.Debugf(c, "Promoted the cached archive of document %q to version %d", newItem.ID, newItem.Version)
	return value.(os.FileInfo), true, nil
}

// rewriteRMDOCMetadata copies the archive at oldPath to newPath with its
// metadata rewritten for item and, when payloadName is set, that entry's
// content replaced by the file at payloadPath.
func rewriteRMDOCMetadata(oldPath, newPath string, item Item, payloadName, payloadPath string) (os.FileInfo, error) {
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
	replaced := false
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
		} else if payloadName != "" && entry.Name == payloadName {
			err = copyFileTo(dst, payloadPath)
			replaced = true
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
	if payloadName != "" && !replaced {
		return nil, fmt.Errorf("cached archive has no %s entry to replace", payloadName)
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

// payload reports the PDF or EPUB a document was imported from. The format is
// the fileType recorded in the document's .content. rmapi parses that field
// while mirroring but neither exposes it nor fills it in for documents it
// uploaded itself, so it is read from the materialized archive instead.
func (c *contentCache) payload(ctx context.Context, item Item) (payloadInfo, error) {
	cachePath, _, err := c.materialize(ctx, item)
	if err != nil {
		return payloadInfo{}, err
	}
	c.mu.Lock()
	info, ok := c.payloads[cachePath]
	c.mu.Unlock()
	if ok {
		return info, nil
	}
	info, err = archivePayload(cachePath, item.ID)
	if err != nil {
		return payloadInfo{}, fmt.Errorf("document %q: %w", item.ID, err)
	}
	if info.fileType == "" {
		fs.Debugf(c, "Document %q version %d has no single PDF or EPUB payload", item.ID, item.Version)
	} else {
		fs.Debugf(c, "Document %q version %d carries a %s payload (%d bytes)", item.ID, item.Version, info.fileType, info.size)
	}
	c.mu.Lock()
	c.payloads[cachePath] = info
	c.mu.Unlock()
	return info, nil
}

// payloadMD5 returns the hex MD5 of a document's PDF or EPUB payload.
func (c *contentCache) payloadMD5(ctx context.Context, item Item, fileType string) (string, error) {
	cachePath, _, err := c.materialize(ctx, item)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	sum, ok := c.hashes[cachePath]
	c.mu.Unlock()
	if ok {
		return sum, nil
	}
	sum, err = archiveEntryMD5(cachePath, payloadEntryName(item.ID, fileType))
	if err != nil {
		return "", err
	}
	fs.Debugf(c, "Hashed the %s payload of document %q version %d", fileType, item.ID, item.Version)
	c.mu.Lock()
	c.hashes[cachePath] = sum
	c.mu.Unlock()
	return sum, nil
}

func copyFileTo(dst io.Writer, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(dst, file)
	return err
}

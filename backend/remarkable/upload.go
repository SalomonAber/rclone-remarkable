package remarkable

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	rmarchive "github.com/juruen/rmapi/archive"
	"github.com/rclone/rclone/lib/readers"
)

var errInvalidRMDOC = errors.New("invalid rmdoc")

func stageRMDOC(ctx context.Context, in io.Reader, tempRoot, visibleName string) (filePath string, size int64, documentID string, err error) {
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		return "", 0, "", err
	}
	tempDir, err := os.MkdirTemp(tempRoot, ".upload-*")
	if err != nil {
		return "", 0, "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tempDir)
		}
	}()

	filePath = filepath.Join(tempDir, visibleName+".rmdoc")
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, "", err
	}
	size, err = io.Copy(file, readers.NewContextReader(ctx, in))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, "", fmt.Errorf("stage rmdoc: %w", err)
	}
	documentID, err = validateRMDOC(filePath)
	if err != nil {
		return "", 0, "", fmt.Errorf("%w: %v", errInvalidRMDOC, err)
	}
	return filePath, size, documentID, nil
}

func validateRMDOC(filePath string) (string, error) {
	archive, err := zip.OpenReader(filePath)
	if err != nil {
		return "", fmt.Errorf("invalid rmdoc ZIP: %w", err)
	}
	defer archive.Close()

	var documentID string
	for _, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, `\`) {
			return "", fmt.Errorf("invalid rmdoc entry path %q", entry.Name)
		}
		if strings.Contains(name, "/") || !strings.HasSuffix(name, ".content") {
			continue
		}
		id := strings.TrimSuffix(name, ".content")
		if _, err := uuid.Parse(id); err != nil {
			return "", fmt.Errorf("invalid rmdoc document UUID %q", id)
		}
		if documentID != "" && documentID != id {
			return "", fmt.Errorf("rmdoc contains multiple document UUIDs")
		}
		documentID = id
	}
	if documentID == "" {
		return "", fmt.Errorf("rmdoc has no top-level UUID.content entry")
	}

	metadataFound := false
	for _, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		if !strings.HasPrefix(name, documentID) {
			return "", fmt.Errorf("rmdoc entry %q does not belong to UUID %q", entry.Name, documentID)
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return "", fmt.Errorf("open rmdoc entry %q: %w", entry.Name, err)
		}
		if name == documentID+".metadata" {
			metadataFound = true
			if entry.UncompressedSize64 > 1<<20 {
				_ = reader.Close()
				return "", fmt.Errorf("rmdoc metadata is too large")
			}
			var metadata rmarchive.MetadataFile
			err = json.NewDecoder(reader).Decode(&metadata)
		} else {
			_, err = io.Copy(io.Discard, reader)
		}
		closeErr := reader.Close()
		if err != nil {
			return "", fmt.Errorf("read rmdoc entry %q: %w", entry.Name, err)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close rmdoc entry %q: %w", entry.Name, closeErr)
		}
	}
	if !metadataFound {
		return "", fmt.Errorf("rmdoc has no matching UUID.metadata entry")
	}
	return documentID, nil
}

func removeStagedRMDOC(filePath string) {
	if filePath != "" {
		_ = os.RemoveAll(filepath.Dir(filePath))
	}
}

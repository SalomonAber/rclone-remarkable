package remarkable

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
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
var errInvalidImport = errors.New("invalid document import")

type stagedDocument struct {
	filePath   string
	size       int64
	documentID string
}

func stageRMDOC(ctx context.Context, in io.Reader, tempRoot, visibleName string) (filePath string, size int64, documentID string, err error) {
	staged, err := stageDocument(ctx, in, tempRoot, visibleName, "rmdoc", errInvalidRMDOC, validateRMDOC)
	if err != nil {
		return "", 0, "", err
	}
	return staged.filePath, staged.size, staged.documentID, nil
}

func stageNativeDocument(ctx context.Context, in io.Reader, tempRoot, visibleName, extension string) (stagedDocument, error) {
	var validate func(string) (string, error)
	switch extension {
	case "pdf":
		validate = func(filePath string) (string, error) { return "", validatePDF(filePath) }
	case "epub":
		validate = func(filePath string) (string, error) { return "", validateEPUB(filePath) }
	default:
		return stagedDocument{}, fmt.Errorf("unsupported document import extension %q", extension)
	}
	return stageDocument(ctx, in, tempRoot, visibleName, extension, errInvalidImport, validate)
}

func stageDocument(ctx context.Context, in io.Reader, tempRoot, visibleName, extension string, validationError error, validate func(string) (string, error)) (staged stagedDocument, err error) {
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		return stagedDocument{}, err
	}
	tempDir, err := os.MkdirTemp(tempRoot, ".upload-*")
	if err != nil {
		return stagedDocument{}, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tempDir)
		}
	}()

	staged.filePath = filepath.Join(tempDir, visibleName+"."+extension)
	file, err := os.OpenFile(staged.filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return stagedDocument{}, err
	}
	staged.size, err = io.Copy(file, readers.NewContextReader(ctx, in))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return stagedDocument{}, fmt.Errorf("stage %s: %w", extension, err)
	}
	staged.documentID, err = validate(staged.filePath)
	if err != nil {
		return stagedDocument{}, fmt.Errorf("%w: %v", validationError, err)
	}
	return staged, nil
}

func validatePDF(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 1024)
	n, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if !bytes.Contains(header[:n], []byte("%PDF-")) {
		return errors.New("missing PDF header")
	}
	return nil
}

type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

func validateEPUB(filePath string) error {
	archive, err := zip.OpenReader(filePath)
	if err != nil {
		return fmt.Errorf("invalid EPUB ZIP: %w", err)
	}
	defer archive.Close()

	entries := make(map[string]bool, len(archive.File))
	var container epubContainer
	mimetypeFound := false
	containerFound := false
	for index, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, `\`) {
			return fmt.Errorf("invalid EPUB entry path %q", entry.Name)
		}
		entries[name] = true
		if entry.FileInfo().IsDir() {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open EPUB entry %q: %w", entry.Name, err)
		}
		switch name {
		case "mimetype":
			if index != 0 || entry.Method != zip.Store {
				_ = reader.Close()
				return errors.New("mimetype must be the first, uncompressed EPUB entry")
			}
			data, readErr := io.ReadAll(io.LimitReader(reader, 256))
			if readErr == nil && string(data) != "application/epub+zip" {
				readErr = errors.New("invalid EPUB mimetype")
			}
			err = readErr
			mimetypeFound = err == nil
		case "META-INF/container.xml":
			if entry.UncompressedSize64 > 1<<20 {
				_ = reader.Close()
				return errors.New("EPUB container.xml is too large")
			}
			err = xml.NewDecoder(reader).Decode(&container)
			if err == nil {
				_, err = io.Copy(io.Discard, reader)
			}
			containerFound = err == nil
		default:
			_, err = io.Copy(io.Discard, reader)
		}
		closeErr := reader.Close()
		if err != nil {
			return fmt.Errorf("read EPUB entry %q: %w", entry.Name, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close EPUB entry %q: %w", entry.Name, closeErr)
		}
	}
	if !mimetypeFound {
		return errors.New("missing EPUB mimetype")
	}
	if !containerFound {
		return errors.New("missing EPUB META-INF/container.xml")
	}
	if len(container.Rootfiles) == 0 {
		return errors.New("EPUB container has no rootfile")
	}
	for _, rootfile := range container.Rootfiles {
		clean := path.Clean(rootfile.FullPath)
		if rootfile.FullPath == "" || clean != rootfile.FullPath || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || !entries[clean] {
			return fmt.Errorf("EPUB rootfile %q is missing or unsafe", rootfile.FullPath)
		}
	}
	return nil
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

func removeStagedDocument(filePath string) {
	if filePath != "" {
		_ = os.RemoveAll(filepath.Dir(filePath))
	}
}

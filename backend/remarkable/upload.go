package remarkable

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
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
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/readers"
)

var errInvalidRMDOC = errors.New("invalid rmdoc")
var errInvalidImport = errors.New("invalid document import")

type stagedDocument struct {
	filePath   string
	size       int64
	documentID string
	md5        string
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
	hasher := md5.New()
	staged.size, err = io.Copy(io.MultiWriter(file, hasher), readers.NewContextReader(ctx, in))
	staged.md5 = hex.EncodeToString(hasher.Sum(nil))
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
	if staged.size == 0 {
		// No document format is valid at zero bytes, and the usual source of
		// one is an application that creates the destination empty and fills
		// it later by renaming its own temporary file over it. Report the
		// condition rclone has a sentinel for instead of a corrupt-document
		// error, so VFS logs it and moves on rather than failing the close.
		err = fmt.Errorf("empty %s source: %w", extension, fs.ErrorCantUploadEmptyFiles)
		return stagedDocument{}, err
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
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
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

// rmdocPayloadEqualsFile reports whether entryName inside the published .rmdoc
// at archivePath is byte-identical to the staged source at filePath.
func rmdocPayloadEqualsFile(archivePath, entryName, filePath string, size int64) (bool, error) {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return false, fmt.Errorf("open published rmdoc %q: %w", archivePath, err)
	}
	defer archive.Close()

	var payload *zip.File
	for _, entry := range archive.File {
		if entry.Name == entryName && !entry.FileInfo().IsDir() {
			payload = entry
			break
		}
	}
	if payload == nil || size < 0 || payload.UncompressedSize64 != uint64(size) {
		return false, nil
	}

	published, err := payload.Open()
	if err != nil {
		return false, fmt.Errorf("open rmdoc entry %q: %w", entryName, err)
	}
	defer published.Close()
	source, err := os.Open(filePath)
	if err != nil {
		return false, err
	}
	defer source.Close()
	return readersEqual(published, source)
}

func readersEqual(left, right io.Reader) (bool, error) {
	leftBuffer := make([]byte, 64*1024)
	rightBuffer := make([]byte, 64*1024)
	atEnd := func(err error) bool {
		return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	}
	for {
		leftRead, leftErr := io.ReadFull(left, leftBuffer)
		rightRead, rightErr := io.ReadFull(right, rightBuffer)
		if leftRead != rightRead || !bytes.Equal(leftBuffer[:leftRead], rightBuffer[:rightRead]) {
			return false, nil
		}
		if leftErr == nil && rightErr == nil {
			continue
		}
		if leftErr != nil && !atEnd(leftErr) {
			return false, leftErr
		}
		if rightErr != nil && !atEnd(rightErr) {
			return false, rightErr
		}
		// Equal-length reads plus a short read on either side means both
		// streams ended here.
		return true, nil
	}
}

func removeStagedDocument(filePath string) {
	if filePath != "" {
		_ = os.RemoveAll(filepath.Dir(filePath))
	}
}

// payloadInfo describes the PDF or EPUB payload of a document. fileType is
// empty for notebooks and any other document without exactly that payload.
type payloadInfo struct {
	fileType string
	size     int64
}

func payloadEntryName(documentID, fileType string) string {
	return documentID + "." + fileType
}

// archivePayload reads the payload of the document documentID from the
// .rmdoc archive at archivePath: the fileType its .content records, provided
// the archive carries that file as <uuid>.<fileType>.
func archivePayload(archivePath, documentID string) (payloadInfo, error) {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return payloadInfo{}, fmt.Errorf("open rmdoc %q: %w", archivePath, err)
	}
	defer archive.Close()

	entries := make(map[string]*zip.File, len(archive.File))
	for _, entry := range archive.File {
		if !entry.FileInfo().IsDir() {
			entries[entry.Name] = entry
		}
	}
	contentEntry := entries[documentID+".content"]
	if contentEntry == nil {
		return payloadInfo{}, nil
	}
	reader, err := contentEntry.Open()
	if err != nil {
		return payloadInfo{}, fmt.Errorf("open %s: %w", contentEntry.Name, err)
	}
	defer reader.Close()
	var content struct {
		FileType string `json:"fileType"`
	}
	if err := json.NewDecoder(reader).Decode(&content); err != nil {
		return payloadInfo{}, fmt.Errorf("read %s: %w", contentEntry.Name, err)
	}
	if content.FileType != "pdf" && content.FileType != "epub" {
		return payloadInfo{}, nil
	}
	payload := entries[payloadEntryName(documentID, content.FileType)]
	if payload == nil {
		return payloadInfo{}, nil
	}
	return payloadInfo{fileType: content.FileType, size: int64(payload.UncompressedSize64)}, nil
}

// openArchiveEntry opens entryName inside the archive at archivePath, starting
// offset bytes in. rmapi stores document files uncompressed, so their bytes
// are read straight from the archive and seeking is free; a compressed entry
// is decompressed and skipped up to offset.
func openArchiveEntry(archivePath, entryName string, offset int64) (io.ReadCloser, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	reader, err := openArchiveFile(file, entryName, offset)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return reader, nil
}

func openArchiveFile(file *os.File, entryName string, offset int64) (io.ReadCloser, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		return nil, fmt.Errorf("open rmdoc %q: %w", file.Name(), err)
	}
	var entry *zip.File
	for _, candidate := range archive.File {
		if candidate.Name == entryName && !candidate.FileInfo().IsDir() {
			entry = candidate
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("rmdoc %q has no %s entry", file.Name(), entryName)
	}
	size := int64(entry.UncompressedSize64)
	if offset < 0 || offset > size {
		return nil, fmt.Errorf("invalid read offset %d", offset)
	}
	if entry.Method == zip.Store {
		dataOffset, err := entry.DataOffset()
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{Reader: io.NewSectionReader(file, dataOffset+offset, size-offset), Closer: file}, nil
	}
	entryReader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, entryReader, offset); err != nil {
		_ = entryReader.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{Reader: entryReader, Closer: closers{entryReader, file}}, nil
}

type closers []io.Closer

func (c closers) Close() error {
	var err error
	for _, closer := range c {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func archiveEntryMD5(archivePath, entryName string) (string, error) {
	reader, err := openArchiveEntry(archivePath, entryName, 0)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	hasher := md5.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

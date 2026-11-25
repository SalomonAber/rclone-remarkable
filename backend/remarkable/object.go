package remarkable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
)

// Object is a reMarkable document, presented as its raw .rmdoc archive or, in
// source_view, as the PDF or EPUB payload it was imported from.
type Object struct {
	fs     *Fs
	remote string
	item   Item
	size   int64
	// payload is only examined in source_view.
	payload payloadInfo
	// md5 is the payload MD5 when it is already known from an upload.
	md5 string
}

func (o *Object) Fs() fs.Info                       { return o.fs }
func (o *Object) String() string                    { return o.remote }
func (o *Object) Remote() string                    { return o.remote }
func (o *Object) ModTime(context.Context) time.Time { return o.item.ModTime }
func (o *Object) Size() int64                       { return o.size }
func (o *Object) Storable() bool                    { return true }

// payloadView reports whether the object presents its PDF or EPUB payload
// rather than the .rmdoc archive.
func (o *Object) payloadView() bool {
	return o.fs.sourceView && o.item.Kind == ItemDocument && payloadNamed(o.item.Name, o.payload.fileType)
}

func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if !o.fs.sourceView || t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	if !o.payloadView() {
		return "", nil
	}
	if o.md5 != "" {
		return o.md5, nil
	}
	sum, err := o.fs.cache.payloadMD5(ctx, o.item, o.payload.fileType)
	if err != nil {
		return "", err
	}
	o.md5 = sum
	return sum, nil
}

// SetModTime records a new modification time in the document metadata. Only
// source_view payloads support it: elsewhere the listed archive is
// synthesized, and its time is the tablet's to keep.
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	if !o.payloadView() {
		return fs.ErrorCantSetModTime
	}
	item, err := o.fs.setModTime(ctx, o.item, modTime)
	if err != nil {
		return err
	}
	o.item = item
	return nil
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	cachePath, _, err := o.fs.cache.materialize(ctx, o.item)
	if err != nil {
		return nil, err
	}

	var offset, limit int64 = 0, -1
	for _, option := range options {
		switch option := option.(type) {
		case *fs.RangeOption:
			offset, limit = option.Decode(o.size)
		case *fs.SeekOption:
			offset = option.Offset
		default:
			if option.Mandatory() {
				return nil, fmt.Errorf("unsupported mandatory open option %v", option)
			}
		}
	}
	if offset < 0 || offset > o.size {
		return nil, fmt.Errorf("invalid read offset %d", offset)
	}

	var reader io.ReadCloser
	if o.payloadView() {
		entryName := payloadEntryName(o.item.ID, o.payload.fileType)
		fs.Debugf(o, "Reading the %s payload of UUID %q from %q at offset %d limit %d", o.payload.fileType, o.item.ID, cachePath, offset, limit)
		if reader, err = openArchiveEntry(cachePath, entryName, offset); err != nil {
			return nil, err
		}
	} else {
		file, err := os.Open(cachePath)
		if err != nil {
			return nil, err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, err
		}
		fs.Debugf(o, "Reading UUID %q from %q at offset %d limit %d", o.item.ID, cachePath, offset, limit)
		reader = file
	}
	if limit >= 0 {
		return struct {
			io.Reader
			io.Closer
		}{Reader: io.LimitReader(reader, limit), Closer: reader}, nil
	}
	return reader, nil
}

// Update replaces the payload of a source_view PDF or EPUB in place. The
// document keeps its UUID, so the tablet keeps its annotations, which are
// tied to page positions rather than to page content. Any other document is
// a compound archive that cannot be replaced safely.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, _ ...fs.OpenOption) error {
	if !o.payloadView() {
		return fserrors.NoRetryError(fmt.Errorf("%w: replacing an existing compound .rmdoc is not supported", fs.ErrorNotImplemented))
	}
	fileType := o.payload.fileType
	if extension := strings.ToLower(strings.TrimPrefix(path.Ext(src.Remote()), ".")); extension != fileType && (extension == "pdf" || extension == "epub" || extension == "rmdoc") {
		fs.Debugf(o, "Refusing to replace the %s payload of UUID %q with %s source %q", fileType, o.item.ID, extension, src.Remote())
		return fserrors.NoRetryError(fmt.Errorf("%w: %q is a %s document and cannot be replaced by a %s", errInvalidImport, o.remote, fileType, extension))
	}

	// Validated as the format already published: an EPUB is refused as the
	// new content of a PDF document whatever its name, and vice versa.
	staged, err := stageNativeDocument(ctx, in, o.fs.uploadTempDir, "replacement", fileType)
	if err != nil {
		if errors.Is(err, fs.ErrorCantUploadEmptyFiles) {
			return err
		}
		if errors.Is(err, errInvalidImport) {
			fs.Debugf(o, "Refusing to replace the %s payload of UUID %q: %v", fileType, o.item.ID, err)
			return fserrors.NoRetryError(err)
		}
		return err
	}
	defer removeStagedDocument(staged.filePath)
	modTime := src.ModTime(ctx)
	entryName := payloadEntryName(o.item.ID, fileType)

	same := false
	if archivePath, _, err := o.fs.cache.materialize(ctx, o.item); err != nil {
		fs.Debugf(o, "Could not compare against the published payload of UUID %q, replacing it: %v", o.item.ID, err)
	} else if same, err = rmdocPayloadEqualsFile(archivePath, entryName, staged.filePath, staged.size); err != nil {
		fs.Debugf(o, "Could not compare against the published payload of UUID %q, replacing it: %v", o.item.ID, err)
	}

	var item Item
	if same {
		// The tablet records its own modification time when a document is
		// opened or annotated; only the time needs to be put back.
		fs.Debugf(o, "The %s payload of UUID %q is unchanged; only updating its modification time", fileType, o.item.ID)
		if item, err = o.fs.setModTime(ctx, o.item, modTime); err != nil {
			return err
		}
	} else {
		fs.Debugf(o, "Replacing the %s payload of UUID %q version %d with %d bytes", fileType, o.item.ID, o.item.Version, staged.size)
		item, err = o.fs.client.Replace(ctx, o.item.ID, staged.filePath, modTime)
		if errors.Is(err, errDocumentUpdateUnsupported) {
			return fserrors.NoRetryError(err)
		}
		if err != nil {
			return fmt.Errorf("replace %s payload of UUID %q: %w", fileType, o.item.ID, err)
		}
		o.fs.promoteCachedArchive(o.item, item, entryName, staged.filePath)
		fs.Infof(o, "Replaced the %s payload of UUID %q; now version %d", fileType, item.ID, item.Version)
	}
	o.item = item
	o.size = staged.size
	o.payload.size = staged.size
	o.md5 = staged.md5
	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	fs.Debugf(o, "Removing UUID %q", o.item.ID)
	return o.fs.client.Remove(ctx, o.item.ID)
}

var _ fs.Object = (*Object)(nil)

package remarkable

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
)

// Object is the future raw .rmdoc representation of a reMarkable document.
type Object struct {
	fs     *Fs
	remote string
	item   Item
	size   int64
}

func (o *Object) Fs() fs.Info                                     { return o.fs }
func (o *Object) String() string                                  { return o.remote }
func (o *Object) Remote() string                                  { return o.remote }
func (o *Object) ModTime(context.Context) time.Time               { return o.item.ModTime }
func (o *Object) Size() int64                                     { return o.size }
func (o *Object) Hash(context.Context, hash.Type) (string, error) { return "", hash.ErrUnsupported }
func (o *Object) Storable() bool                                  { return true }
func (o *Object) SetModTime(context.Context, time.Time) error     { return fs.ErrorCantSetModTime }
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	cachePath, _, err := o.fs.cache.materialize(ctx, o.item)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(cachePath)
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
				_ = file.Close()
				return nil, fmt.Errorf("unsupported mandatory open option %v", option)
			}
		}
	}
	if offset < 0 || offset > o.size {
		_ = file.Close()
		return nil, fmt.Errorf("invalid read offset %d", offset)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	if limit >= 0 {
		return struct {
			io.Reader
			io.Closer
		}{Reader: io.LimitReader(file, limit), Closer: file}, nil
	}
	return file, nil
}
func (o *Object) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return fserrors.NoRetryError(fmt.Errorf("%w: replacing an existing compound .rmdoc is not supported", fs.ErrorNotImplemented))
}
func (o *Object) Remove(ctx context.Context) error {
	return o.fs.client.Remove(ctx, o.item.ID)
}

var _ fs.Object = (*Object)(nil)

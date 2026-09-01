package remarkable

import (
	"context"
	"io"
	"time"

	"github.com/rclone/rclone/fs"
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
func (o *Object) Open(context.Context, ...fs.OpenOption) (io.ReadCloser, error) {
	return nil, fs.ErrorNotImplemented
}
func (o *Object) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return fs.ErrorNotImplemented
}
func (o *Object) Remove(context.Context) error { return fs.ErrorNotImplemented }

var _ fs.Object = (*Object)(nil)

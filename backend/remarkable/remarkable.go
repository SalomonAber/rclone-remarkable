// Package remarkable provides an out-of-tree rclone backend for reMarkable documents.
package remarkable

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "remarkable",
		Description: "reMarkable document tree via rmapi (scaffold only)",
		NewFs:       NewFs,
	})
}

// Fs represents a remarkable remote. Filesystem behavior is intentionally deferred.
type Fs struct {
	name     string
	root     string
	features *fs.Features
	client   Client
}

// NewFs constructs a scaffold filesystem without connecting to rmfakecloud.
func NewFs(ctx context.Context, name, root string, _ configmap.Mapper) (fs.Fs, error) {
	f := &Fs{
		name: name,
		root: strings.Trim(root, "/"),
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	return f, nil
}

func (f *Fs) Name() string             { return f.name }
func (f *Fs) Root() string             { return f.root }
func (f *Fs) String() string           { return fmt.Sprintf("reMarkable root %q", f.root) }
func (f *Fs) Precision() time.Duration { return time.Millisecond }
func (f *Fs) Hashes() hash.Set         { return hash.Set(hash.None) }
func (f *Fs) Features() *fs.Features   { return f.features }

func (f *Fs) List(context.Context, string) (fs.DirEntries, error) {
	return nil, fs.ErrorNotImplemented
}

func (f *Fs) NewObject(context.Context, string) (fs.Object, error) {
	return nil, fs.ErrorNotImplemented
}

func (f *Fs) Put(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) (fs.Object, error) {
	return nil, fs.ErrorNotImplemented
}

func (f *Fs) Mkdir(context.Context, string) error { return fs.ErrorNotImplemented }
func (f *Fs) Rmdir(context.Context, string) error { return fs.ErrorNotImplemented }

var _ fs.Fs = (*Fs)(nil)

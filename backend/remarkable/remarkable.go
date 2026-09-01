// Package remarkable provides an out-of-tree rclone backend for reMarkable documents.
package remarkable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
)

var errClientUnavailable = errors.New("remarkable client is not configured")
var errDestinationExists = errors.New("destination already exists")
var splitConnectionHost = regexp.MustCompile(`^(\[[^]]+\]|[^/:]+):(\d+)(?::(.*))?$`)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "remarkable",
		Description: "reMarkable document tree via rmapi",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name: "host",
			Help: "rmfakecloud or reMarkable API base URL. Defaults to RMAPI_HOST or http://127.0.0.1:7632.",
		}, {
			Name:     "refresh_interval",
			Help:     "Interval between rmapi metadata refreshes while listing.",
			Default:  fs.Duration(30 * time.Second),
			Advanced: true,
		}, {
			Name:     "config",
			Help:     "Path to an rmapi YAML config containing devicetoken and usertoken.",
			Advanced: true,
		}, {
			Name:      "device_token",
			Help:      "rmapi device token. Overrides the config file value.",
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:      "user_token",
			Help:      "rmapi user token. Overrides the config file value.",
			Advanced:  true,
			Sensitive: true,
		}},
	})
}

// Options configures the rmapi library client.
type Options struct {
	Host            string      `config:"host"`
	RefreshInterval fs.Duration `config:"refresh_interval"`
	Config          string      `config:"config"`
	DeviceToken     string      `config:"device_token"`
	UserToken       string      `config:"user_token"`
}

// Fs represents a remarkable document tree.
type Fs struct {
	name     string
	root     string
	rootID   string
	features *fs.Features
	client   Client
	cache    *contentCache
}

// NewFs constructs a filesystem backed by a configured rmapi sync 1.5 client.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if opt.Host == "" {
		opt.Host = os.Getenv("RMAPI_HOST")
	}
	if opt.Host == "" {
		opt.Host = "http://127.0.0.1:7632"
	}
	if opt.Config == "" {
		opt.Config = os.Getenv("RMAPI_CONFIG")
	}
	opt.Host, root = normalizeConnectionHost(opt.Host, root)
	client, err := newConfiguredRMAPIClient(*opt)
	if err != nil {
		return nil, err
	}
	return newFs(ctx, name, root, client, filepath.Join(config.GetCacheDir(), "remarkable"))
}

// normalizeConnectionHost repairs unescaped http:// URLs in rclone connection strings.
// Rclone treats the ':' after "http" as the option/root separator.
func normalizeConnectionHost(host, root string) (string, string) {
	if (host != "http" && host != "https") || !strings.HasPrefix(root, "//") {
		return host, root
	}
	matches := splitConnectionHost.FindStringSubmatch(strings.TrimPrefix(root, "//"))
	if matches == nil {
		return host, root
	}
	endpoint := host + "://" + matches[1] + ":" + matches[2]
	return endpoint, matches[3]
}

func newFs(ctx context.Context, name, root string, client Client, cacheDir string) (*Fs, error) {
	f := &Fs{name: name, root: strings.Trim(root, "/"), client: client}
	if client != nil {
		f.cache = newContentCache(cacheDir, client)
		rootItem, err := f.resolve(ctx, "", f.root)
		if err != nil {
			return nil, err
		}
		if rootItem.Kind != ItemDirectory {
			return nil, fs.ErrorIsFile
		}
		f.rootID = rootItem.ID
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

func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if f.client == nil {
		return nil, errClientUnavailable
	}
	directory, err := f.resolve(ctx, f.rootID, dir)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}
	if directory.Kind != ItemDirectory {
		return nil, fs.ErrorDirNotFound
	}
	items, err := f.client.List(ctx, directory.ID)
	if err != nil {
		return nil, err
	}
	if err := checkDuplicateNames(items); err != nil {
		return nil, err
	}

	entries := make(fs.DirEntries, 0, len(items))
	for _, item := range items {
		remote := path.Join(dir, localName(item))
		if item.Kind == ItemDirectory {
			entries = append(entries, fs.NewDir(remote, item.ModTime).SetID(item.ID))
			continue
		}
		object, err := f.newObject(ctx, remote, item)
		if err != nil {
			return nil, err
		}
		entries = append(entries, object)
	}
	return entries, nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.client == nil {
		return nil, errClientUnavailable
	}
	item, err := f.resolve(ctx, f.rootID, remote)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return nil, fs.ErrorObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	if item.Kind != ItemDocument {
		return nil, fs.ErrorObjectNotFound
	}
	return f.newObject(ctx, remote, item)
}

func (f *Fs) Put(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) (fs.Object, error) {
	return nil, fs.ErrorNotImplemented
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if f.client == nil {
		return errClientUnavailable
	}
	if dir == "" {
		return nil
	}
	parentID, name, err := f.destination(ctx, dir, ItemDirectory, "")
	if errors.Is(err, errDestinationExists) {
		item, resolveErr := f.resolve(ctx, f.rootID, dir)
		if resolveErr == nil && item.Kind == ItemDirectory {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	_, err = f.client.Mkdir(ctx, parentID, name)
	return err
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if f.client == nil {
		return errClientUnavailable
	}
	if dir == "" {
		return fs.ErrorPermissionDenied
	}
	item, err := f.resolve(ctx, f.rootID, dir)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return fs.ErrorDirNotFound
	}
	if err != nil {
		return err
	}
	if item.Kind != ItemDirectory {
		return fs.ErrorDirNotFound
	}
	children, err := f.client.List(ctx, item.ID)
	if err != nil {
		return err
	}
	if len(children) != 0 {
		return fs.ErrorDirectoryNotEmpty
	}
	return f.client.Remove(ctx, item.ID)
}

// Move renames or moves an object by mutating metadata on its existing UUID.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObject, ok := src.(*Object)
	if !ok || srcObject.fs.Name() != f.Name() {
		return nil, fs.ErrorCantMove
	}
	parentID, name, err := f.destination(ctx, remote, ItemDocument, srcObject.item.ID)
	if err != nil {
		return nil, err
	}
	item, err := f.client.Move(ctx, srcObject.item.ID, parentID, name)
	if err != nil {
		return nil, err
	}
	size := srcObject.size
	if info, promoted, err := f.cache.promoteMetadata(srcObject.item, item); err != nil {
		fs.Errorf(srcObject, "Failed to promote cached metadata: %v", err)
	} else if promoted {
		size = info.Size()
	}
	return &Object{fs: f, remote: remote, item: item, size: size}, nil
}

// DirMove moves a collection by mutating its parent/name metadata once.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok || srcFs.Name() != f.Name() {
		return fs.ErrorCantDirMove
	}
	if srcRemote == "" {
		return fs.ErrorCantDirMove
	}
	item, err := srcFs.resolve(ctx, srcFs.rootID, srcRemote)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return fs.ErrorDirNotFound
	}
	if err != nil {
		return err
	}
	if item.Kind != ItemDirectory {
		return fs.ErrorDirNotFound
	}
	parentID, name, err := f.destination(ctx, dstRemote, ItemDirectory, item.ID)
	if errors.Is(err, errDestinationExists) {
		return fs.ErrorDirExists
	}
	if err != nil {
		return err
	}
	_, err = f.client.Move(ctx, item.ID, parentID, name)
	return err
}

func (f *Fs) newObject(ctx context.Context, remote string, item Item) (*Object, error) {
	_, info, err := f.cache.materialize(ctx, item)
	if err != nil {
		return nil, err
	}
	return &Object{fs: f, remote: remote, item: item, size: info.Size()}, nil
}

func (f *Fs) resolve(ctx context.Context, parentID, remote string) (Item, error) {
	current := Item{ID: parentID, Kind: ItemDirectory}
	if remote == "" {
		return current, nil
	}
	for _, component := range strings.Split(remote, "/") {
		items, err := f.client.List(ctx, current.ID)
		if err != nil {
			return Item{}, err
		}
		if err := checkDuplicateNames(items); err != nil {
			return Item{}, err
		}
		found := false
		for _, item := range items {
			if localName(item) == component {
				current = item
				found = true
				break
			}
		}
		if !found {
			return Item{}, fs.ErrorObjectNotFound
		}
	}
	return current, nil
}

func localName(item Item) string {
	if item.Kind == ItemDocument {
		return item.Name + ".rmdoc"
	}
	return item.Name
}

func checkDuplicateNames(items []Item) error {
	seen := make(map[string]string, len(items))
	for _, item := range items {
		name := localName(item)
		if id, ok := seen[name]; ok {
			return fmt.Errorf("duplicate visible name %q for UUIDs %q and %q", name, id, item.ID)
		}
		seen[name] = item.ID
	}
	return nil
}

func (f *Fs) destination(ctx context.Context, remote string, kind ItemKind, sourceID string) (parentID, name string, err error) {
	remote = strings.Trim(remote, "/")
	name = path.Base(remote)
	parentRemote := path.Dir(remote)
	if parentRemote == "." {
		parentRemote = ""
	}
	if name == "." || name == "" {
		return "", "", fmt.Errorf("invalid destination %q", remote)
	}
	if kind == ItemDocument {
		if !strings.HasSuffix(name, ".rmdoc") {
			return "", "", fmt.Errorf("document destination %q must end in .rmdoc", remote)
		}
		name = strings.TrimSuffix(name, ".rmdoc")
		if name == "" {
			return "", "", fmt.Errorf("document visible name must not be empty")
		}
	}
	parent, err := f.resolve(ctx, f.rootID, parentRemote)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return "", "", fs.ErrorDirNotFound
	}
	if err != nil {
		return "", "", err
	}
	if parent.Kind != ItemDirectory {
		return "", "", fs.ErrorDirNotFound
	}
	items, err := f.client.List(ctx, parent.ID)
	if err != nil {
		return "", "", err
	}
	if err := checkDuplicateNames(items); err != nil {
		return "", "", err
	}
	wanted := name
	if kind == ItemDocument {
		wanted += ".rmdoc"
	}
	for _, item := range items {
		if localName(item) == wanted && item.ID != sourceID {
			return "", "", fmt.Errorf("%w: %q", errDestinationExists, remote)
		}
	}
	return parent.ID, name, nil
}

var (
	_ fs.Fs       = (*Fs)(nil)
	_ fs.Mover    = (*Fs)(nil)
	_ fs.DirMover = (*Fs)(nil)
)

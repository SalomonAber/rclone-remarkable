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
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
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
			Help:     "Fallback interval between rmapi metadata refreshes while listing.",
			Default:  fs.Duration(30 * time.Second),
			Advanced: true,
		}, {
			Name:     "config",
			Help:     "Path to an rmapi YAML config containing devicetoken and usertoken.",
			Advanced: true,
		}, {
			Name:      "client_cert",
			Help:      "Path to a PEM-encoded TLS client certificate. Must be used with client_key.",
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:      "client_key",
			Help:      "Path to the PEM-encoded private key for client_cert. Must be used with client_cert.",
			Advanced:  true,
			Sensitive: true,
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
	ClientCert      string      `config:"client_cert"`
	ClientKey       string      `config:"client_key"`
	DeviceToken     string      `config:"device_token"`
	UserToken       string      `config:"user_token"`
	// OnUserTokenRefresh receives a refreshed token after initialization succeeds.
	OnUserTokenRefresh func(string) error `config:"-"`
}

// Fs represents a remarkable document tree.
type Fs struct {
	name          string
	root          string
	rootID        string
	rootMissing   bool
	rootMu        sync.Mutex
	features      *fs.Features
	client        Client
	cache         *contentCache
	uploadTempDir string
	notifyMu      sync.Mutex
	notifyNextID  uint64
	notifiers     map[uint64]func(string, fs.EntryType)
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
	rcloneCacheDir := config.GetCacheDir()
	client, err := NewConfiguredRMAPIClient(*opt, rmapiMetadataCacheRoot(rcloneCacheDir))
	if err != nil {
		return nil, err
	}
	return newFs(ctx, name, root, client, filepath.Join(rcloneCacheDir, "remarkable"))
}

func rmapiMetadataCacheRoot(rcloneCacheDir string) string {
	if rcloneCacheDir == "" {
		return ""
	}
	userCacheDir, err := os.UserCacheDir()
	if err != nil || userCacheDir == "" {
		return filepath.Join(rcloneCacheDir, "remarkable-metadata")
	}
	if filepath.Clean(rcloneCacheDir) == filepath.Join(filepath.Clean(userCacheDir), "rclone") {
		return ""
	}
	return filepath.Join(rcloneCacheDir, "remarkable-metadata")
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
	f := &Fs{
		name:          name,
		root:          strings.Trim(root, "/"),
		client:        client,
		uploadTempDir: filepath.Join(cacheDir, ".uploads"),
	}
	var rootErr error
	if client != nil {
		f.cache = newContentCache(cacheDir, client)
		rootItem, err := f.resolve(ctx, "", f.root)
		if errors.Is(err, fs.ErrorObjectNotFound) {
			f.rootMissing = true
			fs.Debugf(f, "Root does not exist yet; it is created on the first write")
		} else if err != nil {
			return nil, err
		} else if rootItem.Kind != ItemDirectory {
			fs.Debugf(f, "Root is document UUID %q; continuing from its parent", rootItem.ID)
			parentRoot := path.Dir(f.root)
			if parentRoot == "." {
				parentRoot = ""
			}
			f.root = parentRoot
			f.rootID = rootItem.ParentID
			rootErr = fs.ErrorIsFile
		} else {
			f.rootID = rootItem.ID
			fs.Debugf(f, "Root resolved to UUID %q", rootItem.ID)
		}
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	return f, rootErr
}

func (f *Fs) Name() string             { return f.name }
func (f *Fs) Root() string             { return f.root }
func (f *Fs) String() string           { return fmt.Sprintf("reMarkable root %q", f.root) }
func (f *Fs) Precision() time.Duration { return time.Millisecond }
func (f *Fs) Hashes() hash.Set         { return hash.Set(hash.None) }
func (f *Fs) Features() *fs.Features   { return f.features }

// ChangeNotify polls rmapi for remote metadata changes and invalidates every
// known directory in the mounted tree when its sync root changes. rmapi does
// not expose a per-entry change feed, so invalidating directories is the only
// reliable way to cover moves, deletions, and changes below already-cached
// subdirectories.
func (f *Fs) ChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	f.notifyMu.Lock()
	if f.notifiers == nil {
		f.notifiers = make(map[uint64]func(string, fs.EntryType))
	}
	notifierID := f.notifyNextID
	f.notifyNextID++
	f.notifiers[notifierID] = notifyFunc
	f.notifyMu.Unlock()
	fs.Debugf(f, "ChangeNotify notifier %d registered", notifierID)

	go func() {
		var ticker *time.Ticker
		var tickerC <-chan time.Time
		defer func() {
			if ticker != nil {
				ticker.Stop()
			}
			f.notifyMu.Lock()
			delete(f.notifiers, notifierID)
			f.notifyMu.Unlock()
			fs.Debugf(f, "ChangeNotify notifier %d stopped", notifierID)
		}()

		for {
			select {
			case interval, ok := <-pollIntervalChan:
				if !ok {
					return
				}
				if ticker != nil {
					ticker.Stop()
					ticker, tickerC = nil, nil
				}
				if interval > 0 {
					ticker = time.NewTicker(interval)
					tickerC = ticker.C
					fs.Debugf(f, "ChangeNotify polling every %s", interval)
				} else {
					fs.Debugf(f, "ChangeNotify polling disabled")
				}
			case <-tickerC:
				changed, err := f.client.Refresh(ctx)
				if err != nil {
					fs.Errorf(f, "ChangeNotify metadata refresh failed: %v", err)
					continue
				}
				if changed {
					fs.Debugf(f, "Remote sync root changed; invalidating the mounted directory tree")
					f.notifyDirectoryTree(ctx, notifyFunc)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// notifyChange immediately invalidates mounted VFS directory caches after a
// local mutation. Polling remains necessary for mutations made by the tablet
// or another client, but waiting for the next poll after our own PDF/EPUB
// import would leave the write-only source name visible unnecessarily.
func (f *Fs) notifyChange(remote string, entryType fs.EntryType) {
	f.notifyMu.Lock()
	notifiers := make([]func(string, fs.EntryType), 0, len(f.notifiers))
	for _, notify := range f.notifiers {
		notifiers = append(notifiers, notify)
	}
	f.notifyMu.Unlock()
	fs.Debugf(f, "Invalidating %q for %d VFS notifier(s)", remote, len(notifiers))
	for _, notify := range notifiers {
		notify(remote, entryType)
	}
}

// nativeImportNotifyDelays covers the whole window in which VFS can still
// consider the import-only source in use, not just the first moments after Put.
var nativeImportNotifyDelays = []time.Duration{
	250 * time.Millisecond,
	time.Second,
	3 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// notifyNativeImport invalidates the parent directory immediately and then on a
// decaying schedule. While the import-only source (for example Report.pdf) is
// in use, a directory read preserves its virtual entry and consumes the
// pending invalidation, so both it and Report.pdf.rmdoc stay visible until
// --dir-cache-time expires. VFS holds that reference during Put and keeps it
// for as long as anything else has the source open afterwards, which on a
// desktop can be a thumbnailer or indexer reacting to the new file. Each repeat
// only marks one directory stale, which is far cheaper than the stale listing
// it prevents.
func (f *Fs) notifyNativeImport(remote string) {
	fs.Debugf(f, "Repeating the invalidation of %q at %v after the native import", remote, nativeImportNotifyDelays)
	f.notifyChange(remote, fs.EntryObject)
	for _, delay := range nativeImportNotifyDelays {
		time.AfterFunc(delay, func() {
			f.notifyChange(remote, fs.EntryObject)
		})
	}
}

func (f *Fs) notifyDirectoryTree(ctx context.Context, notifyFunc func(string, fs.EntryType)) {
	notifyFunc("", fs.EntryDirectory)
	seen := map[string]bool{f.rootID: true}
	var walk func(string, string)
	walk = func(parentID, remote string) {
		items, err := f.client.List(ctx, parentID)
		if err != nil {
			fs.Errorf(f, "ChangeNotify directory traversal failed at %q: %v", remote, err)
			return
		}
		for _, item := range items {
			if item.Kind != ItemDirectory || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			childRemote := path.Join(remote, localName(item))
			notifyFunc(childRemote, fs.EntryDirectory)
			walk(item.ID, childRemote)
		}
	}
	walk(f.rootID, "")
	fs.Debugf(f, "Invalidated the root and %d subdirectories", len(seen)-1)
}

func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if f.client == nil {
		return nil, errClientUnavailable
	}
	if err := f.ensureRoot(ctx, false); err != nil {
		return nil, err
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
	fs.Debugf(f, "Listed %d entries in %q (UUID %q)", len(entries), dir, directory.ID)
	return entries, nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.client == nil {
		return nil, errClientUnavailable
	}
	if err := f.ensureRoot(ctx, false); err != nil {
		return nil, fs.ErrorObjectNotFound
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

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, _ ...fs.OpenOption) (fs.Object, error) {
	if f.client == nil {
		return nil, errClientUnavailable
	}
	if err := f.ensureRoot(ctx, true); err != nil {
		return nil, err
	}
	canonicalRemote, extension, err := importRemote(src.Remote())
	if err != nil {
		return nil, fserrors.NoRetryError(err)
	}
	fs.Debugf(f, "Importing %s source %q as %q", extension, src.Remote(), canonicalRemote)
	parentID, visibleName, conflict, destErr := f.resolveDestination(ctx, canonicalRemote, ItemDocument, "")
	if destErr != nil && !errors.Is(destErr, errDestinationExists) {
		return nil, destErr
	}
	var staged stagedDocument
	if extension == "rmdoc" {
		filePath, size, documentID, stageErr := stageRMDOC(ctx, in, f.uploadTempDir, visibleName)
		staged = stagedDocument{filePath: filePath, size: size, documentID: documentID}
		err = stageErr
	} else {
		staged, err = stageNativeDocument(ctx, in, f.uploadTempDir, visibleName, extension)
	}
	if err != nil {
		if errors.Is(err, errInvalidRMDOC) || errors.Is(err, errInvalidImport) {
			return nil, fserrors.NoRetryError(err)
		}
		return nil, err
	}
	defer removeStagedDocument(staged.filePath)
	fs.Debugf(f, "Staged %s source of %q (%d bytes, embedded UUID %q)", extension, canonicalRemote, staged.size, staged.documentID)
	if destErr != nil {
		return f.adoptRepeatedImport(ctx, canonicalRemote, extension, staged, conflict, destErr)
	}
	if staged.documentID != "" {
		if existing, getErr := f.client.Get(ctx, staged.documentID); getErr == nil {
			return nil, fserrors.NoRetryError(fmt.Errorf("%w: UUID %q is already visible as %q", errDestinationExists, staged.documentID, localName(existing)))
		} else if !errors.Is(getErr, errItemNotFound) {
			return nil, getErr
		}
	}

	item, err := f.client.Upload(ctx, parentID, staged.filePath)
	if err != nil {
		landed, lookupErr := f.landedImport(ctx, parentID, visibleName, staged.documentID)
		if lookupErr != nil {
			fs.Errorf(f, "Could not check whether the failed %s import of %q landed: %v", extension, canonicalRemote, lookupErr)
		}
		if landed == nil {
			fs.Debugf(f, "No published document matches the failed %s import of %q; reporting the upload error", extension, canonicalRemote)
			return nil, fmt.Errorf("upload %s: %w", extension, err)
		}
		fs.Infof(f, "Import of %q reported %v but landed as UUID %q; adopting the published document", canonicalRemote, err, landed.ID)
		item = *landed
	}
	if staged.documentID != "" && item.ID != staged.documentID {
		return nil, fmt.Errorf("uploaded UUID %q does not match source UUID %q", item.ID, staged.documentID)
	}
	item.Name = visibleName
	item.ParentID = parentID
	fs.Debugf(f, "Imported %q as UUID %q version %d", canonicalRemote, item.ID, item.Version)
	if extension != "rmdoc" {
		// The VFS wrote the import-only source name (for example Report.pdf),
		// while listings expose Report.pdf.rmdoc. Mark its parent stale until
		// VFS has finished releasing the transient source entry.
		f.notifyNativeImport(canonicalRemote)
	}
	// rmapi synthesizes or normalizes the remote archive while importing, so
	// its .rmdoc size is unknown until that representation is materialized.
	return &Object{fs: f, remote: canonicalRemote, item: item, size: -1}, nil
}

// adoptRepeatedImport resolves an import whose visible name is already taken.
// rclone cannot match an import-only source (Report.pdf) against its published
// representation (Report.pdf.rmdoc), so every copy, sync, or drag-and-drop
// re-sends a document that is already on the tablet. Refusing that outright
// also wedges a mount: VFS retries the rejected writeback forever and leaves
// the source name visible next to the .rmdoc. An identical repeat is therefore
// reported as already done. A different payload is still refused, because
// replacing a published document is not supported.
func (f *Fs) adoptRepeatedImport(ctx context.Context, canonicalRemote, extension string, staged stagedDocument, conflict Item, destErr error) (fs.Object, error) {
	if conflict.Kind != ItemDocument {
		fs.Debugf(f, "Refusing the import of %q: collection UUID %q occupies that name", canonicalRemote, conflict.ID)
		return nil, fserrors.NoRetryError(destErr)
	}
	fs.Debugf(f, "%q is occupied by UUID %q; checking whether this import is a repeat", canonicalRemote, conflict.ID)
	same, err := f.importMatchesDocument(ctx, conflict, staged, extension)
	if err != nil {
		// Without the published payload there is no way to tell a repeat from a
		// genuine collision, so keep refusing rather than report a document we
		// could not verify as already imported.
		fs.Errorf(f, "Could not compare %q against published UUID %q: %v", canonicalRemote, conflict.ID, err)
		return nil, fserrors.NoRetryError(destErr)
	}
	if !same {
		fs.Debugf(f, "Refusing the import of %q: published UUID %q holds a different payload", canonicalRemote, conflict.ID)
		return nil, fserrors.NoRetryError(destErr)
	}
	fs.Infof(f, "%q is already published as UUID %q; skipping the duplicate upload", canonicalRemote, conflict.ID)
	if extension != "rmdoc" {
		f.notifyNativeImport(canonicalRemote)
	}
	// Report an unknown size for the same reason the upload path does: the
	// published .rmdoc is not the source, so a known size here would trip
	// rclone's post-transfer size check.
	return &Object{fs: f, remote: canonicalRemote, item: conflict, size: -1}, nil
}

// importMatchesDocument reports whether a published document is the same
// import as the freshly staged source. rmapi stores a PDF or EPUB source
// verbatim inside the published .rmdoc as <uuid>.<ext>, so the comparison is
// against that embedded payload. An .rmdoc source carries its own document
// UUID, which identifies it directly; re-importing one UUID with changed
// contents could not be applied anyway, since Update rejects replacement.
func (f *Fs) importMatchesDocument(ctx context.Context, item Item, staged stagedDocument, extension string) (bool, error) {
	if extension == "rmdoc" {
		return staged.documentID != "" && staged.documentID == item.ID, nil
	}
	archivePath, _, err := f.cache.materialize(ctx, item)
	if err != nil {
		return false, err
	}
	return rmdocPayloadEqualsFile(archivePath, item.ID+"."+extension, staged.filePath, staged.size)
}

// landedImport reports the document an interrupted import created, if any.
// rmapi can fail after the server has committed the upload, for example when
// its response is lost. Leaving that unexamined wedges the import: the document
// is on the tablet, every retry is refused as a duplicate destination, and a
// mount keeps retrying the rejected writeback forever.
func (f *Fs) landedImport(ctx context.Context, parentID, visibleName, documentID string) (*Item, error) {
	if _, err := f.client.Refresh(ctx); err != nil {
		return nil, err
	}
	items, err := f.client.List(ctx, parentID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Kind != ItemDocument || item.Name != visibleName {
			continue
		}
		if documentID != "" && item.ID != documentID {
			continue
		}
		return &item, nil
	}
	return nil, nil
}

func importRemote(remote string) (canonicalRemote, extension string, err error) {
	extension = strings.ToLower(strings.TrimPrefix(path.Ext(remote), "."))
	switch extension {
	case "rmdoc":
		return remote, extension, nil
	case "pdf", "epub":
		base := strings.TrimSuffix(remote, path.Ext(remote))
		if path.Base(base) == "" || path.Base(base) == "." {
			return "", "", fmt.Errorf("document visible name must not be empty")
		}
		// Preserve the source format in the durable visible name. Besides making
		// provenance clear, this prevents File.pdf and File.epub from both
		// collapsing to File.rmdoc. Normalize the source suffix for canonical,
		// case-insensitive names.
		return base + "." + extension + ".rmdoc", extension, nil
	default:
		return "", "", fmt.Errorf("unsupported document import %q: destination must end in .pdf, .epub, or .rmdoc", remote)
	}
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if f.client == nil {
		return errClientUnavailable
	}
	if dir == "" {
		return f.ensureRoot(ctx, true)
	}
	if err := f.ensureRoot(ctx, true); err != nil {
		return err
	}
	parentID, name, err := f.destination(ctx, dir, ItemDirectory, "")
	if errors.Is(err, errDestinationExists) {
		item, resolveErr := f.resolve(ctx, f.rootID, dir)
		if resolveErr == nil && item.Kind == ItemDirectory {
			fs.Debugf(f, "Collection %q already exists as UUID %q", dir, item.ID)
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
	if err := f.ensureRoot(ctx, false); err != nil {
		return err
	}
	if dir == "" && f.root == "" {
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
	fs.Debugf(f, "Removing empty collection %q (UUID %q)", dir, item.ID)
	return f.client.Remove(ctx, item.ID)
}

func (f *Fs) ensureRoot(ctx context.Context, create bool) error {
	f.rootMu.Lock()
	defer f.rootMu.Unlock()
	if !f.rootMissing {
		return nil
	}
	current := Item{Kind: ItemDirectory}
	for _, component := range strings.Split(f.root, "/") {
		items, err := f.client.List(ctx, current.ID)
		if err != nil {
			return err
		}
		found := false
		for _, item := range items {
			if item.Kind == ItemDirectory && item.Name == component {
				current = item
				found = true
				break
			}
		}
		if found {
			continue
		}
		if !create {
			return fs.ErrorDirNotFound
		}
		current, err = f.client.Mkdir(ctx, current.ID, component)
		if err != nil {
			return err
		}
	}
	f.rootID = current.ID
	f.rootMissing = false
	fs.Debugf(f, "Root is now UUID %q", current.ID)
	return nil
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
	fs.Debugf(f, "Moved %q to %q (UUID %q)", srcObject.remote, remote, item.ID)
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
	if _, err = f.client.Move(ctx, item.ID, parentID, name); err != nil {
		return err
	}
	fs.Debugf(f, "Moved collection %q to %q (UUID %q)", srcRemote, dstRemote, item.ID)
	return nil
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
	parentID, name, _, err = f.resolveDestination(ctx, remote, kind, sourceID)
	return parentID, name, err
}

// resolveDestination validates a destination path and, when errDestinationExists
// is returned, also reports the item occupying it along with the resolved parent
// and visible name. An import needs those to tell a repeat of a document it
// already published from a genuine collision with an unrelated one.
func (f *Fs) resolveDestination(ctx context.Context, remote string, kind ItemKind, sourceID string) (parentID, name string, conflict Item, err error) {
	remote = strings.Trim(remote, "/")
	name = path.Base(remote)
	parentRemote := path.Dir(remote)
	if parentRemote == "." {
		parentRemote = ""
	}
	if name == "." || name == "" {
		return "", "", Item{}, fmt.Errorf("invalid destination %q", remote)
	}
	if kind == ItemDocument {
		if !strings.HasSuffix(name, ".rmdoc") {
			return "", "", Item{}, fmt.Errorf("document destination %q must end in .rmdoc", remote)
		}
		name = strings.TrimSuffix(name, ".rmdoc")
		if name == "" {
			return "", "", Item{}, fmt.Errorf("document visible name must not be empty")
		}
	}
	parent, err := f.resolve(ctx, f.rootID, parentRemote)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return "", "", Item{}, fs.ErrorDirNotFound
	}
	if err != nil {
		return "", "", Item{}, err
	}
	if parent.Kind != ItemDirectory {
		return "", "", Item{}, fs.ErrorDirNotFound
	}
	items, err := f.client.List(ctx, parent.ID)
	if err != nil {
		return "", "", Item{}, err
	}
	if err := checkDuplicateNames(items); err != nil {
		return "", "", Item{}, err
	}
	wanted := name
	if kind == ItemDocument {
		wanted += ".rmdoc"
	}
	for _, item := range items {
		if localName(item) == wanted && item.ID != sourceID {
			return parent.ID, name, item, fmt.Errorf("%w: %q", errDestinationExists, remote)
		}
	}
	return parent.ID, name, Item{}, nil
}

var (
	_ fs.Fs             = (*Fs)(nil)
	_ fs.Mover          = (*Fs)(nil)
	_ fs.DirMover       = (*Fs)(nil)
	_ fs.ChangeNotifier = (*Fs)(nil)
)

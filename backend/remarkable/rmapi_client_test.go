package remarkable

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rmapi "github.com/juruen/rmapi/api"
	rmconfig "github.com/juruen/rmapi/config"
	"github.com/juruen/rmapi/filetree"
	"github.com/juruen/rmapi/model"
	"github.com/juruen/rmapi/transport"
)

// fakeAPICtx implements rmapi.ApiCtx directly on top of a real
// filetree.FileTreeCtx so tests can exercise rmapiClient (the adapter around
// rmapi's concrete API) without any network access, including the tree
// shape rmapi itself always produces (e.g. its synthetic "trash" collection).
type fakeAPICtx struct {
	tree              *filetree.FileTreeCtx
	refreshHash       string
	refreshGeneration int64
	refreshErr        error
}

func (f *fakeAPICtx) Filetree() *filetree.FileTreeCtx    { return f.tree }
func (f *fakeAPICtx) FetchDocument(string, string) error { return nil }
func (f *fakeAPICtx) CreateDir(string, string, bool) (*model.Document, error) {
	return nil, nil
}
func (f *fakeAPICtx) UploadDocument(string, string, bool, *int, *int, *int, *string) (*model.Document, error) {
	return nil, nil
}
func (f *fakeAPICtx) ReplaceDocumentFile(string, string, bool) error { return nil }
func (f *fakeAPICtx) MoveEntry(*model.Node, *model.Node, string) (*model.Node, error) {
	return nil, nil
}
func (f *fakeAPICtx) DeleteEntry(*model.Node, bool, bool) error { return nil }
func (f *fakeAPICtx) SyncComplete() error                       { return nil }
func (f *fakeAPICtx) Nuke() error                               { return nil }
func (f *fakeAPICtx) Refresh() (string, int64, error) {
	return f.refreshHash, f.refreshGeneration, f.refreshErr
}

var _ rmapi.ApiCtx = (*fakeAPICtx)(nil)

func TestRMAPIRefreshDetectsSyncRootChanges(t *testing.T) {
	tree := filetree.CreateFileTreeCtx()
	api := &fakeAPICtx{tree: &tree, refreshHash: "hash-1", refreshGeneration: 1}
	client := &rmapiClient{api: api, refreshInterval: time.Hour, lastRefresh: time.Now()}

	changed, err := client.Refresh(context.Background())
	if err != nil || !changed {
		t.Fatalf("first refresh = changed %v, error %v; want changed", changed, err)
	}
	changed, err = client.Refresh(context.Background())
	if err != nil || changed {
		t.Fatalf("unchanged refresh = changed %v, error %v; want unchanged", changed, err)
	}
	api.refreshGeneration++
	changed, err = client.Refresh(context.Background())
	if err != nil || !changed {
		t.Fatalf("new generation refresh = changed %v, error %v; want changed", changed, err)
	}
	api.refreshErr = fmt.Errorf("refresh failed")
	if _, err := client.Refresh(context.Background()); err == nil {
		t.Fatal("refresh error was not returned")
	}
}

func TestRMAPIUserTokenRefreshSuccess(t *testing.T) {
	originalCreate := createRMAPIContext
	originalRefresh := refreshRMAPIUserToken
	t.Cleanup(func() {
		createRMAPIContext = originalCreate
		refreshRMAPIUserToken = originalRefresh
	})

	createCalls := 0
	createRMAPIContext = func(httpCtx *transport.HttpClientCtx, _ rmapi.SyncVersion, _ rmapi.Options) (rmapi.ApiCtx, error) {
		createCalls++
		if createCalls == 1 {
			return nil, transport.ErrUnauthorized
		}
		if httpCtx.Tokens.UserToken != "refreshed-user-token" {
			t.Fatalf("retry user token was not replaced")
		}
		tree := filetree.CreateFileTreeCtx()
		return &fakeAPICtx{tree: &tree}, nil
	}
	refreshCalls := 0
	refreshRMAPIUserToken = func(httpCtx *transport.HttpClientCtx) (string, error) {
		refreshCalls++
		if httpCtx.Tokens.DeviceToken != "valid-device-token" {
			t.Fatal("refresh did not use the configured device token")
		}
		return "refreshed-user-token", nil
	}
	var callbackToken string
	client, err := NewConfiguredRMAPIClient(Options{
		Host:        "https://cloud.example",
		DeviceToken: "valid-device-token",
		UserToken:   "expired-user-token",
		OnUserTokenRefresh: func(token string) error {
			callbackToken = token
			return nil
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || createCalls != 2 || refreshCalls != 1 {
		t.Fatalf("client=%v create calls=%d refresh calls=%d", client, createCalls, refreshCalls)
	}
	if callbackToken != "refreshed-user-token" {
		t.Fatal("refresh callback did not receive the new token")
	}
}

func TestRMAPIUserTokenRefreshRejectedDeviceToken(t *testing.T) {
	originalCreate := createRMAPIContext
	originalRefresh := refreshRMAPIUserToken
	t.Cleanup(func() {
		createRMAPIContext = originalCreate
		refreshRMAPIUserToken = originalRefresh
	})

	createCalls := 0
	createRMAPIContext = func(_ *transport.HttpClientCtx, _ rmapi.SyncVersion, _ rmapi.Options) (rmapi.ApiCtx, error) {
		createCalls++
		return nil, transport.ErrUnauthorized
	}
	refreshCalls := 0
	refreshRMAPIUserToken = func(_ *transport.HttpClientCtx) (string, error) {
		refreshCalls++
		return "", transport.ErrUnauthorized
	}

	_, err := NewConfiguredRMAPIClient(Options{
		Host:        "https://cloud.example",
		DeviceToken: "rejected-device-token",
		UserToken:   "expired-user-token",
	}, "")
	if err == nil || !strings.Contains(err.Error(), "device re-registration is required") {
		t.Fatalf("error = %v", err)
	}
	if createCalls != 1 || refreshCalls != 1 {
		t.Fatalf("create calls=%d refresh calls=%d", createCalls, refreshCalls)
	}
	if strings.Contains(err.Error(), "rejected-device-token") || strings.Contains(err.Error(), "expired-user-token") {
		t.Fatalf("error leaks a token: %v", err)
	}
}

func TestRMAPIUserTokenRefreshRetryLimit(t *testing.T) {
	originalCreate := createRMAPIContext
	originalRefresh := refreshRMAPIUserToken
	t.Cleanup(func() {
		createRMAPIContext = originalCreate
		refreshRMAPIUserToken = originalRefresh
	})

	createCalls := 0
	createRMAPIContext = func(_ *transport.HttpClientCtx, _ rmapi.SyncVersion, _ rmapi.Options) (rmapi.ApiCtx, error) {
		createCalls++
		return nil, transport.ErrUnauthorized
	}
	refreshCalls := 0
	refreshRMAPIUserToken = func(_ *transport.HttpClientCtx) (string, error) {
		refreshCalls++
		return "still-rejected-user-token", nil
	}

	_, err := NewConfiguredRMAPIClient(Options{
		Host:        "https://cloud.example",
		DeviceToken: "valid-device-token",
		UserToken:   "expired-user-token",
	}, "")
	if err == nil || !strings.Contains(err.Error(), "refreshed user token was rejected") {
		t.Fatalf("error = %v", err)
	}
	if createCalls != 2 || refreshCalls != 1 {
		t.Fatalf("create calls=%d refresh calls=%d", createCalls, refreshCalls)
	}
	if strings.Contains(err.Error(), "still-rejected-user-token") {
		t.Fatalf("error leaks refreshed token: %v", err)
	}
}

func TestRMAPIExplicitCacheWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	rcloneCacheDir := t.TempDir()
	metadataRoot := rmapiMetadataCacheRoot(rcloneCacheDir)
	wantRoot := filepath.Join(rcloneCacheDir, "remarkable-metadata")
	if metadataRoot != wantRoot {
		t.Fatalf("metadata cache root = %q, want %q", metadataRoot, wantRoot)
	}

	originalCreate := createRMAPIContext
	t.Cleanup(func() { createRMAPIContext = originalCreate })
	var gotOptions rmapi.Options
	createRMAPIContext = func(_ *transport.HttpClientCtx, _ rmapi.SyncVersion, options rmapi.Options) (rmapi.ApiCtx, error) {
		gotOptions = options
		tree := filetree.CreateFileTreeCtx()
		return &fakeAPICtx{tree: &tree}, nil
	}

	opt := Options{Host: "https://cloud.example", UserToken: "test-user-token"}
	if _, err := NewConfiguredRMAPIClient(opt, metadataRoot); err != nil {
		t.Fatalf("NewConfiguredRMAPIClient without HOME/XDG_CACHE_HOME: %v", err)
	}
	accountID := sha256.Sum256([]byte(opt.Host + "\x00" + opt.UserToken))
	wantCacheDir := filepath.Join(wantRoot, fmt.Sprintf("%x", accountID))
	if gotOptions.CacheDir != wantCacheDir {
		t.Fatalf("rmapi cache dir = %q, want %q", gotOptions.CacheDir, wantCacheDir)
	}
}

func TestRMAPIUsesRcloneFallbackWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	rcloneFallback := filepath.Join(os.TempDir(), "rclone")
	want := filepath.Join(rcloneFallback, "remarkable-metadata")
	if got := rmapiMetadataCacheRoot(rcloneFallback); got != want {
		t.Fatalf("fallback metadata cache root = %q, want %q", got, want)
	}
}

func TestRMAPIExplicitCacheIsDeterministicAndAccountSeparated(t *testing.T) {
	metadataRoot := rmapiMetadataCacheRoot(filepath.Join(t.TempDir(), "explicit-rclone-cache"))
	first := sha256.Sum256([]byte("https://first.example\x00first-token"))
	second := sha256.Sum256([]byte("https://second.example\x00second-token"))
	firstPath := filepath.Join(metadataRoot, fmt.Sprintf("%x", first))
	secondPath := filepath.Join(metadataRoot, fmt.Sprintf("%x", second))
	if firstPath == secondPath || filepath.Dir(firstPath) != metadataRoot || filepath.Dir(secondPath) != metadataRoot {
		t.Fatalf("account cache paths are not separated: %q and %q", firstPath, secondPath)
	}
}

func TestRMAPIDefaultCachePreservesInteractiveBehavior(t *testing.T) {
	xdgCacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdgCacheHome)
	t.Setenv("HOME", t.TempDir())

	if got := rmapiMetadataCacheRoot(filepath.Join(xdgCacheHome, "rclone")); got != "" {
		t.Fatalf("default interactive metadata root = %q, want rmapi default", got)
	}

	originalCreate := createRMAPIContext
	t.Cleanup(func() { createRMAPIContext = originalCreate })
	var gotOptions rmapi.Options
	createRMAPIContext = func(_ *transport.HttpClientCtx, _ rmapi.SyncVersion, options rmapi.Options) (rmapi.ApiCtx, error) {
		gotOptions = options
		tree := filetree.CreateFileTreeCtx()
		return &fakeAPICtx{tree: &tree}, nil
	}
	if _, err := NewConfiguredRMAPIClient(Options{Host: "https://cloud.example", UserToken: "test-user-token"}, ""); err != nil {
		t.Fatal(err)
	}
	if gotOptions.CacheDir != "" {
		t.Fatalf("interactive rmapi cache dir = %q, want empty default selector", gotOptions.CacheDir)
	}
}

// TestRealClientListHidesSyntheticTrash proves that rmapi's always-present
// "trash" collection (filetree.CreateFileTreeCtx adds it as a child of root
// unconditionally) is not exposed to rclone as a browsable/addressable
// top-level directory. Before the fix, this synthetic node leaked into every
// root listing, which both (a) crashes any account that has a real top-level
// folder literally named "trash" via checkDuplicateNames, and (b) lets
// Mkdir/Move address it by name, moving real documents into rmapi's actual
// deletion mechanism.
func TestRealClientListHidesSyntheticTrash(t *testing.T) {
	tree := filetree.CreateFileTreeCtx()
	tree.AddDocument(&model.Document{ID: "work", Name: "Work", Type: model.DirectoryType})
	client := &rmapiClient{api: &fakeAPICtx{tree: &tree}, refreshInterval: time.Hour, lastRefresh: time.Now()}

	items, err := client.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == filetree.TrashID {
			t.Fatalf("synthetic rmapi trash collection exposed at root: %#v", item)
		}
	}
	if len(items) != 1 || items[0].ID != "work" {
		t.Fatalf("root listing = %#v, want only the real Work folder", items)
	}
}

// TestFsListDoesNotCollideWithSyntheticTrash exercises the same defect one
// layer up: an account with a real top-level folder named "trash" must still
// be listable. Without the fix, rmapi's synthetic trash node and the real
// folder both present the local name "trash" to checkDuplicateNames, and
// Fs.List fails entirely.
func TestFsListDoesNotCollideWithSyntheticTrash(t *testing.T) {
	tree := filetree.CreateFileTreeCtx()
	tree.AddDocument(&model.Document{ID: "real-trash-folder", Name: "trash", Type: model.DirectoryType})
	client := &rmapiClient{api: &fakeAPICtx{tree: &tree}, refreshInterval: time.Hour, lastRefresh: time.Now()}

	backend, err := newFs(context.Background(), "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := backend.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List errored due to rmapi's synthetic trash collection: %v", err)
	}
	assertEntry(t, entries, "trash", true)
	if len(entries) != 1 {
		t.Fatalf("root entries = %#v, want exactly the real 'trash' folder", entries)
	}
}

// TestRMAPIHostConfigurationSerializedAcrossClients proves that rmapi's
// process-wide host configuration (mutated by configureRMAPIHost, read from
// package-level vars by rmapi's own HTTP calls) can no longer be observed
// half-configured for the wrong client. A slow client A holds rmapiHostMu for
// its entire request; a concurrent client B configuring a different host must
// block until A is done, so A's request is guaranteed to see only its own
// host's endpoints.
func TestRMAPIHostConfigurationSerializedAcrossClients(t *testing.T) {
	const hostA = "http://host-a.example"
	const hostB = "http://host-b.example"

	started := make(chan struct{})
	release := make(chan struct{})
	seenDuringA := make(chan string, 1)
	seenAfterB := make(chan string, 1)

	go func() {
		rmapiHostMu.Lock()
		defer rmapiHostMu.Unlock()
		configureRMAPIHost(hostA)
		close(started)
		<-release
		seenDuringA <- rmconfig.BlobUrl
	}()

	<-started
	done := make(chan struct{})
	go func() {
		// This must block on rmapiHostMu until client A's goroutine above
		// releases it, so it can never observe or cause a mid-request host
		// change for client A.
		rmapiHostMu.Lock()
		defer rmapiHostMu.Unlock()
		configureRMAPIHost(hostB)
		seenAfterB <- rmconfig.BlobUrl
		close(done)
	}()

	close(release)
	if got := <-seenDuringA; got != hostA+"/sync/v3/files/" {
		t.Fatalf("client A observed host = %q, want %q", got, hostA)
	}
	<-done
	if got := <-seenAfterB; got != hostB+"/sync/v3/files/" {
		t.Fatalf("client B observed host = %q, want %q", got, hostB)
	}
}

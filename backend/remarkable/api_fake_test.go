package remarkable

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/juruen/rmapi/model"
	"github.com/rclone/rclone/fs"
)

type fakeClient struct {
	mu              sync.Mutex
	items           map[string]Item
	contents        map[string][]byte
	downloads       map[string]int
	uploads         []uploadCall
	uploadErrors    []error
	moves           []moveCall
	mkdirs          []mkdirCall
	removes         []string
	refreshes       int
	refreshChanged  bool
	refreshErr      error
	nextID          int
	downloadStarted chan struct{}
	releaseDownload chan struct{}
}

type moveCall struct {
	ID       string
	ParentID string
	Name     string
}

type uploadCall struct {
	ParentID   string
	SourcePath string
}

type mkdirCall struct {
	ParentID string
	Name     string
}

func (c *fakeClient) List(_ context.Context, parentID string) ([]Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var items []Item
	for _, item := range c.items {
		if item.ParentID == parentID {
			items = append(items, item)
		}
	}
	return items, nil
}

func (c *fakeClient) Refresh(_ context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshes++
	changed := c.refreshChanged
	c.refreshChanged = false
	return changed, c.refreshErr
}

func (c *fakeClient) Get(_ context.Context, id string) (Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[id]
	if !ok {
		return Item{}, fmt.Errorf("%w: %q", errItemNotFound, id)
	}
	return item, nil
}

func (c *fakeClient) Download(ctx context.Context, id string, dst io.Writer) error {
	c.mu.Lock()
	if c.downloads == nil {
		c.downloads = make(map[string]int)
	}
	c.downloads[id]++
	content, ok := c.contents[id]
	started := c.downloadStarted
	release := c.releaseDownload
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("content %q not found", id)
	}
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := dst.Write(content)
	return err
}
func (c *fakeClient) Upload(_ context.Context, parentID, sourcePath string) (Item, error) {
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(sourcePath), "."))
	var documentID string
	var err error
	switch extension {
	case "rmdoc":
		documentID, err = validateRMDOC(sourcePath)
	case "pdf":
		err = validatePDF(sourcePath)
		documentID = uuid.NewString()
	case "epub":
		err = validateEPUB(sourcePath)
		documentID = uuid.NewString()
	default:
		err = fmt.Errorf("unsupported fake upload extension %q", extension)
	}
	if err != nil {
		return Item{}, err
	}
	var remoteContent []byte
	if extension == "rmdoc" {
		remoteContent, err = os.ReadFile(sourcePath)
	} else {
		remoteContent, err = fakeRMDOC(documentID)
	}
	if err != nil {
		return Item{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uploads = append(c.uploads, uploadCall{ParentID: parentID, SourcePath: sourcePath})
	if len(c.uploadErrors) > 0 {
		err := c.uploadErrors[0]
		c.uploadErrors = c.uploadErrors[1:]
		if err != nil {
			return Item{}, err
		}
	}
	name := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
	item := Item{ID: documentID, Name: name, ParentID: parentID, Kind: ItemDocument, Version: 1}
	if c.items == nil {
		c.items = make(map[string]Item)
	}
	if c.contents == nil {
		c.contents = make(map[string][]byte)
	}
	c.items[item.ID] = item
	c.contents[item.ID] = remoteContent
	return item, nil
}

func fakeRMDOC(documentID string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	metadata, err := writer.Create(documentID + ".metadata")
	if err != nil {
		return nil, err
	}
	if _, err := metadata.Write([]byte(`{"visibleName":"imported","type":"DocumentType"}`)); err != nil {
		return nil, err
	}
	content, err := writer.Create(documentID + ".content")
	if err != nil {
		return nil, err
	}
	if _, err := content.Write([]byte(`{}`)); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
func (c *fakeClient) Move(_ context.Context, id, parentID, name string) (Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[id]
	if !ok {
		return Item{}, fmt.Errorf("item %q not found", id)
	}
	c.moves = append(c.moves, moveCall{ID: id, ParentID: parentID, Name: name})
	item.ParentID = parentID
	item.Name = name
	item.Version++
	item.ModTime = time.Now().UTC()
	c.items[id] = item
	return item, nil
}
func (c *fakeClient) Mkdir(_ context.Context, parentID, name string) (Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	item := Item{
		ID:       fmt.Sprintf("created-%d", c.nextID),
		Name:     name,
		ParentID: parentID,
		Kind:     ItemDirectory,
	}
	c.mkdirs = append(c.mkdirs, mkdirCall{ParentID: parentID, Name: name})
	if c.items == nil {
		c.items = make(map[string]Item)
	}
	c.items[item.ID] = item
	return item, nil
}
func (c *fakeClient) Remove(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[id]; !ok {
		return fmt.Errorf("item %q not found", id)
	}
	c.removes = append(c.removes, id)
	delete(c.items, id)
	return nil
}

func (c *fakeClient) downloadCount(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.downloads[id]
}

func (c *fakeClient) operations() ([]moveCall, []mkdirCall, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]moveCall(nil), c.moves...), append([]mkdirCall(nil), c.mkdirs...), append([]string(nil), c.removes...)
}

var _ Client = (*fakeClient)(nil)

func TestBackendRegistered(t *testing.T) {
	info, err := fs.Find("remarkable")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "remarkable" {
		t.Fatalf("registered backend name = %q", info.Name)
	}
}

func TestChangeNotifyRefreshesAndInvalidatesDirectoryTree(t *testing.T) {
	client := &fakeClient{
		items: map[string]Item{
			"folder": {ID: "folder", Name: "Folder", Kind: ItemDirectory},
			"nested": {ID: "nested", Name: "Nested", ParentID: "folder", Kind: ItemDirectory},
			"doc":    {ID: "doc", Name: "Document", ParentID: "nested", Kind: ItemDocument},
		},
		refreshChanged: true,
	}
	backend, err := newFs(context.Background(), "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if backend.Features().ChangeNotify == nil {
		t.Fatal("ChangeNotify feature was not advertised")
	}
	intervals := make(chan time.Duration, 1)
	notifications := make(chan string, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend.ChangeNotify(ctx, func(remote string, entryType fs.EntryType) {
		if entryType != fs.EntryDirectory {
			t.Errorf("notification type = %v, want directory", entryType)
		}
		notifications <- remote
	}, intervals)
	intervals <- time.Millisecond

	want := map[string]bool{"": true, "Folder": true, "Folder/Nested": true}
	for len(want) > 0 {
		select {
		case remote := <-notifications:
			if !want[remote] {
				t.Fatalf("unexpected notification for %q", remote)
			}
			delete(want, remote)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for notifications; missing %v", want)
		}
	}
	intervals <- 0
	close(intervals)

	client.mu.Lock()
	refreshes := client.refreshes
	client.mu.Unlock()
	if refreshes == 0 {
		t.Fatal("metadata was not refreshed")
	}
}

func TestItemFromDocument(t *testing.T) {
	doc := &model.Document{
		ID:             "document-id",
		Name:           "Notebook",
		Parent:         "parent-id",
		Type:           model.DocumentType,
		Version:        7,
		ModifiedClient: "2026-09-01T08:30:00.123Z",
	}

	item, err := itemFromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != doc.ID || item.Name != doc.Name || item.ParentID != doc.Parent {
		t.Fatalf("item identity = %#v", item)
	}
	if item.Kind != ItemDocument || item.Version != doc.Version {
		t.Fatalf("item metadata = %#v", item)
	}
	wantTime := time.Date(2026, time.September, 1, 8, 30, 0, 123_000_000, time.UTC)
	if !item.ModTime.Equal(wantTime) {
		t.Fatalf("modification time = %s, want %s", item.ModTime, wantTime)
	}
}

func TestRMAPIConfigTokensAndOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rmapi.conf")
	if err := os.WriteFile(configPath, []byte("devicetoken: from-file-device\nusertoken: from-file-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens, err := rmapiTokens(Options{Config: configPath, UserToken: "explicit-user"})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.DeviceToken != "from-file-device" || tokens.UserToken != "explicit-user" {
		t.Fatalf("tokens = %#v", tokens)
	}
}

func TestNormalizeConnectionHost(t *testing.T) {
	tests := []struct {
		host, root, wantHost, wantRoot string
	}{
		{host: "http", root: "//127.0.0.1:7632", wantHost: "http://127.0.0.1:7632", wantRoot: ""},
		{host: "http", root: "//127.0.0.1:7632:Work/Existing.rmdoc", wantHost: "http://127.0.0.1:7632", wantRoot: "Work/Existing.rmdoc"},
		{host: "https://cloud.example", root: "Work", wantHost: "https://cloud.example", wantRoot: "Work"},
	}
	for _, test := range tests {
		host, root := normalizeConnectionHost(test.host, test.root)
		if host != test.wantHost || root != test.wantRoot {
			t.Fatalf("normalizeConnectionHost(%q, %q) = (%q, %q)", test.host, test.root, host, root)
		}
	}
}

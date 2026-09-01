package remarkable

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/juruen/rmapi/model"
	"github.com/rclone/rclone/fs"
)

type fakeClient struct {
	mu              sync.Mutex
	items           map[string]Item
	contents        map[string][]byte
	downloads       map[string]int
	downloadStarted chan struct{}
	releaseDownload chan struct{}
}

func (c *fakeClient) List(_ context.Context, parentID string) ([]Item, error) {
	var items []Item
	for _, item := range c.items {
		if item.ParentID == parentID {
			items = append(items, item)
		}
	}
	return items, nil
}

func (c *fakeClient) Get(_ context.Context, id string) (Item, error) {
	item, ok := c.items[id]
	if !ok {
		return Item{}, fmt.Errorf("item %q not found", id)
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
func (c *fakeClient) Move(context.Context, string, string, string) (Item, error) {
	return Item{}, nil
}
func (c *fakeClient) Mkdir(context.Context, string, string) (Item, error) {
	return Item{}, nil
}
func (c *fakeClient) Remove(context.Context, string) error { return nil }

func (c *fakeClient) downloadCount(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.downloads[id]
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

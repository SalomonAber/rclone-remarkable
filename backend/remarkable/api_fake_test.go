package remarkable

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/juruen/rmapi/model"
	"github.com/rclone/rclone/fs"
)

type fakeClient struct {
	items map[string]Item
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
	return c.items[id], nil
}

func (c *fakeClient) Download(context.Context, string, io.Writer) error { return nil }
func (c *fakeClient) Move(context.Context, string, string, string) (Item, error) {
	return Item{}, nil
}
func (c *fakeClient) Mkdir(context.Context, string, string) (Item, error) {
	return Item{}, nil
}
func (c *fakeClient) Remove(context.Context, string) error { return nil }

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

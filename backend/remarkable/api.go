package remarkable

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	rmapi "github.com/juruen/rmapi/api"
	"github.com/juruen/rmapi/model"
)

// Item is the backend's representation of a reMarkable tree entry.
type Item struct {
	ID       string
	Name     string
	ParentID string
	Kind     ItemKind
	Version  int
	ModTime  time.Time
}

// ItemKind distinguishes documents from collections.
type ItemKind int

const (
	ItemDocument ItemKind = iota
	ItemDirectory
)

// Client isolates rclone filesystem behavior from rmapi's concrete API.
type Client interface {
	List(ctx context.Context, parentID string) ([]Item, error)
	Get(ctx context.Context, id string) (Item, error)
	Download(ctx context.Context, id string, dst io.Writer) error
	Move(ctx context.Context, id, parentID, name string) (Item, error)
	Mkdir(ctx context.Context, parentID, name string) (Item, error)
	Remove(ctx context.Context, id string) error
}

type rmapiClient struct {
	api rmapi.ApiCtx
}

func newRMAPIClient(api rmapi.ApiCtx) Client {
	return &rmapiClient{api: api}
}

func (c *rmapiClient) List(_ context.Context, parentID string) ([]Item, error) {
	parent := c.api.Filetree().NodeById(parentID)
	if parent == nil || !parent.IsDirectory() {
		return nil, fmt.Errorf("rmapi: parent %q is not a directory", parentID)
	}

	items := make([]Item, 0, len(parent.Children))
	for _, node := range parent.Children {
		item, err := itemFromNode(node)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (c *rmapiClient) Get(_ context.Context, id string) (Item, error) {
	node := c.api.Filetree().NodeById(id)
	if node == nil {
		return Item{}, fmt.Errorf("rmapi: item %q not found", id)
	}
	return itemFromNode(node)
}

func (c *rmapiClient) Download(_ context.Context, id string, dst io.Writer) error {
	tmp, err := os.CreateTemp("", "rclone-remarkable-*.rmdoc")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		return err
	}
	defer os.Remove(name)

	if err := c.api.FetchDocument(id, name); err != nil {
		return err
	}
	src, err := os.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(dst, src)
	return err
}

func (c *rmapiClient) Move(_ context.Context, id, parentID, name string) (Item, error) {
	src := c.api.Filetree().NodeById(id)
	dst := c.api.Filetree().NodeById(parentID)
	if src == nil || dst == nil {
		return Item{}, fmt.Errorf("rmapi: move source or destination not found")
	}
	node, err := c.api.MoveEntry(src, dst, name)
	if err != nil {
		return Item{}, err
	}
	c.api.Filetree().MoveNode(src, node)
	return itemFromNode(node)
}

func (c *rmapiClient) Mkdir(_ context.Context, parentID, name string) (Item, error) {
	doc, err := c.api.CreateDir(parentID, name, true)
	if err != nil {
		return Item{}, err
	}
	c.api.Filetree().AddDocument(doc)
	return itemFromDocument(doc)
}

func (c *rmapiClient) Remove(_ context.Context, id string) error {
	node := c.api.Filetree().NodeById(id)
	if node == nil {
		return fmt.Errorf("rmapi: item %q not found", id)
	}
	if err := c.api.DeleteEntry(node, false, true); err != nil {
		return err
	}
	c.api.Filetree().DeleteNode(node)
	return nil
}

func itemFromNode(node *model.Node) (Item, error) {
	if node == nil || node.Document == nil {
		return Item{}, fmt.Errorf("rmapi: invalid node")
	}
	return itemFromDocument(node.Document)
}

func itemFromDocument(doc *model.Document) (Item, error) {
	kind := ItemDocument
	if doc.Type == model.DirectoryType {
		kind = ItemDirectory
	}

	var modTime time.Time
	var err error
	if doc.ModifiedClient != "" {
		modTime, err = time.Parse(time.RFC3339Nano, doc.ModifiedClient)
		if err != nil {
			return Item{}, fmt.Errorf("rmapi: parse modification time: %w", err)
		}
	}
	return Item{
		ID:       doc.ID,
		Name:     doc.Name,
		ParentID: doc.Parent,
		Kind:     kind,
		Version:  doc.Version,
		ModTime:  modTime,
	}, nil
}

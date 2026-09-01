package remarkable

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	rmapi "github.com/juruen/rmapi/api"
	rmconfig "github.com/juruen/rmapi/config"
	"github.com/juruen/rmapi/filetree"
	"github.com/juruen/rmapi/model"
	"github.com/juruen/rmapi/transport"
	"gopkg.in/yaml.v2"
)

var errItemNotFound = errors.New("item not found")

var createRMAPIContext = rmapi.CreateApiCtxWithOptions

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
	Upload(ctx context.Context, parentID, sourcePath string) (Item, error)
	Move(ctx context.Context, id, parentID, name string) (Item, error)
	Mkdir(ctx context.Context, parentID, name string) (Item, error)
	Remove(ctx context.Context, id string) error
}

type rmapiClient struct {
	mu              sync.Mutex
	api             rmapi.ApiCtx
	host            string
	refreshInterval time.Duration
	lastRefresh     time.Time
}

// rmapiHostMu serializes rmapi's process-wide endpoint configuration
// (mutated by configureRMAPIHost) with the request that depends on it. rmapi
// stores its API endpoints in package-level variables rather than per-client
// state, so multiple *rmapiClient instances (e.g. two configured "remarkable"
// remotes pointing at different hosts) share that global state. Without this
// lock, constructing or using a second client can silently repoint an
// in-flight or subsequent request from a different client at the wrong host.
var rmapiHostMu sync.Mutex

func newRMAPIClient(api rmapi.ApiCtx, host string, refreshInterval time.Duration) Client {
	return &rmapiClient{api: api, host: host, refreshInterval: refreshInterval, lastRefresh: time.Now()}
}

func newConfiguredRMAPIClient(opt Options, metadataCacheRoot string) (Client, error) {
	httpCtx, err := newRMAPIHTTPClientCtx(opt)
	if err != nil {
		return nil, err
	}
	tokens, err := rmapiTokens(opt)
	if err != nil {
		return nil, err
	}
	if tokens.UserToken == "" {
		return nil, fmt.Errorf("remarkable: user token is required; set user_token or provide config with usertoken")
	}

	// The host is (re)applied immediately before each request under
	// rmapiHostMu (see below); CreateApiCtx itself only needs a host to be
	// configured for its initial tree mirror, so apply it once here too.
	rmapiHostMu.Lock()
	configureRMAPIHost(opt.Host)
	httpCtx.Tokens = tokens
	metadataCacheDir := ""
	if metadataCacheRoot != "" {
		accountID := sha256.Sum256([]byte(opt.Host + "\x00" + tokens.UserToken))
		metadataCacheDir = filepath.Join(metadataCacheRoot, fmt.Sprintf("%x", accountID))
	}
	apiCtx, err := createRMAPIContext(&httpCtx, rmapi.Version15, rmapi.Options{CacheDir: metadataCacheDir})
	rmapiHostMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("remarkable: initialize rmapi sync client: %w", err)
	}
	refreshInterval := time.Duration(opt.RefreshInterval)
	if refreshInterval <= 0 {
		refreshInterval = 30 * time.Second
	}
	return newRMAPIClient(apiCtx, opt.Host, refreshInterval), nil
}

// newRMAPIHTTPClientCtx configures rmapi's private HTTP client.
func newRMAPIHTTPClientCtx(opt Options) (transport.HttpClientCtx, error) {
	if (opt.ClientCert == "") != (opt.ClientKey == "") {
		return transport.HttpClientCtx{}, errors.New("remarkable: client_cert and client_key must be set together")
	}

	httpCtx := transport.CreateHttpClientCtx(model.AuthTokens{})
	if opt.ClientCert == "" {
		return httpCtx, nil
	}

	certificate, err := tls.LoadX509KeyPair(opt.ClientCert, opt.ClientKey)
	if err != nil {
		return transport.HttpClientCtx{}, errors.New("remarkable: unable to load client certificate/key")
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return transport.HttpClientCtx{}, errors.New("remarkable: unable to configure client certificate transport")
	}
	transportConfig := defaultTransport.Clone()
	if transportConfig.TLSClientConfig == nil {
		transportConfig.TLSClientConfig = new(tls.Config)
	} else {
		transportConfig.TLSClientConfig = transportConfig.TLSClientConfig.Clone()
	}
	transportConfig.TLSClientConfig.Certificates = append(transportConfig.TLSClientConfig.Certificates, certificate)
	httpCtx.Client.Transport = transportConfig
	return httpCtx, nil
}

func rmapiTokens(opt Options) (model.AuthTokens, error) {
	tokens := model.AuthTokens{
		DeviceToken: opt.DeviceToken,
		UserToken:   opt.UserToken,
	}
	if opt.Config == "" {
		return tokens, nil
	}
	data, err := os.ReadFile(opt.Config)
	if err != nil {
		return model.AuthTokens{}, fmt.Errorf("remarkable: read rmapi config %q: %w", opt.Config, err)
	}
	var fileTokens model.AuthTokens
	if err := yaml.Unmarshal(data, &fileTokens); err != nil {
		return model.AuthTokens{}, fmt.Errorf("remarkable: parse rmapi config %q: %w", opt.Config, err)
	}
	if tokens.DeviceToken == "" {
		tokens.DeviceToken = fileTokens.DeviceToken
	}
	if tokens.UserToken == "" {
		tokens.UserToken = fileTokens.UserToken
	}
	return tokens, nil
}

func configureRMAPIHost(host string) {
	host = strings.TrimRight(host, "/")
	rmconfig.NewTokenDevice = host + "/token/json/2/device/new"
	rmconfig.NewUserDevice = host + "/token/json/2/user/new"
	rmconfig.ListDocs = host + "/document-storage/json/2/docs"
	rmconfig.UpdateStatus = host + "/document-storage/json/2/upload/update-status"
	rmconfig.UploadRequest = host + "/document-storage/json/2/upload/request"
	rmconfig.DeleteEntry = host + "/document-storage/json/2/delete"
	rmconfig.UploadBlob = host + "/sync/v2/signed-urls/uploads"
	rmconfig.DownloadBlob = host + "/sync/v2/signed-urls/downloads"
	rmconfig.SyncComplete = host + "/sync/v2/sync-complete"
	rmconfig.BlobUrl = host + "/sync/v3/files/"
	rmconfig.RootGet = host + "/sync/v4/root"
	rmconfig.RootPut = host + "/sync/v3/root"
}

func (c *rmapiClient) List(_ context.Context, parentID string) ([]Item, error) {
	rmapiHostMu.Lock()
	configureRMAPIHost(c.host)
	defer rmapiHostMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.lastRefresh) >= c.refreshInterval {
		if _, _, err := c.api.Refresh(); err != nil {
			return nil, fmt.Errorf("rmapi: refresh file tree: %w", err)
		}
		c.lastRefresh = time.Now()
	}
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
		if parentID == "" && item.ID == filetree.TrashID {
			// rmapi always synthesizes a "trash" collection as a child of
			// the root. It is not a real user directory: exposing it here
			// would (a) collide with, and break listing of, an account that
			// happens to have a real top-level folder named "trash", and
			// (b) let Mkdir/Move address it by name, which would move real
			// documents into rmapi's actual delete/trash mechanism.
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

func (c *rmapiClient) Get(_ context.Context, id string) (Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	node := c.api.Filetree().NodeById(id)
	if node == nil {
		return Item{}, fmt.Errorf("%w: %q", errItemNotFound, id)
	}
	return itemFromNode(node)
}

func (c *rmapiClient) Download(_ context.Context, id string, dst io.Writer) error {
	rmapiHostMu.Lock()
	configureRMAPIHost(c.host)
	defer rmapiHostMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
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

func (c *rmapiClient) Upload(_ context.Context, parentID, sourcePath string) (Item, error) {
	rmapiHostMu.Lock()
	configureRMAPIHost(c.host)
	defer rmapiHostMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	doc, err := c.api.UploadDocument(parentID, sourcePath, true, nil, nil, nil, nil)
	if err != nil {
		return Item{}, err
	}
	c.api.Filetree().AddDocument(doc)
	return itemFromDocument(doc)
}

func (c *rmapiClient) Move(_ context.Context, id, parentID, name string) (Item, error) {
	rmapiHostMu.Lock()
	configureRMAPIHost(c.host)
	defer rmapiHostMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
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
	rmapiHostMu.Lock()
	configureRMAPIHost(c.host)
	defer rmapiHostMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	doc, err := c.api.CreateDir(parentID, name, true)
	if err != nil {
		return Item{}, err
	}
	c.api.Filetree().AddDocument(doc)
	return itemFromDocument(doc)
}

func (c *rmapiClient) Remove(_ context.Context, id string) error {
	rmapiHostMu.Lock()
	configureRMAPIHost(c.host)
	defer rmapiHostMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
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

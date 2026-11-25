package remarkable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	fsobject "github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/sync"
)

func TestRMFakecloudIntegration(t *testing.T) {
	if os.Getenv("REMARKABLE_INTEGRATION") != "1" {
		t.Skip("set REMARKABLE_INTEGRATION=1 to run against rmfakecloud")
	}

	host := os.Getenv("REMARKABLE_HOST")
	if host == "" {
		host = os.Getenv("RMAPI_HOST")
	}
	if host == "" {
		host = "http://127.0.0.1:7632"
	}
	configPath := os.Getenv("REMARKABLE_CONFIG")
	if configPath == "" {
		configPath = os.Getenv("RMAPI_CONFIG")
	}
	if configPath == "" && os.Getenv("REMARKABLE_USER_TOKEN") == "" {
		t.Fatal("set REMARKABLE_CONFIG/RMAPI_CONFIG or REMARKABLE_USER_TOKEN")
	}

	ctx := context.Background()
	client, err := NewConfiguredRMAPIClient(Options{
		Host:            host,
		RefreshInterval: fs.Duration(time.Second),
		Config:          configPath,
		ClientCert:      os.Getenv("REMARKABLE_CLIENT_CERT"),
		ClientKey:       os.Getenv("REMARKABLE_CLIENT_KEY"),
		DeviceToken:     os.Getenv("REMARKABLE_DEVICE_TOKEN"),
		UserToken:       os.Getenv("REMARKABLE_USER_TOKEN"),
	}, filepath.Join(t.TempDir(), "rmapi-metadata"))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := newFs(ctx, "integration", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.List(ctx, ""); err != nil {
		t.Fatalf("list root: %v", err)
	}

	suffix := time.Now().UTC().Format("20060102T150405.000000000")
	folderName := "rclone-remarkable-integration-" + suffix
	folder, err := client.Mkdir(ctx, "", folderName)
	if err != nil {
		t.Fatalf("create test folder: %v", err)
	}
	defer func() {
		if err := client.Remove(ctx, folder.ID); err != nil {
			t.Errorf("remove test folder %q: %v", folderName, err)
		}
	}()

	documentPath := filepath.Join(t.TempDir(), "controlled.rmdoc")
	const documentID = "865ee31e-5b86-47cd-a850-f2b2ec1af72d"
	if err := os.WriteFile(documentPath, validRMDOC(t, documentID, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	documentFile, err := os.Open(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	documentInfo, err := documentFile.Stat()
	if err != nil {
		_ = documentFile.Close()
		t.Fatal(err)
	}
	source := fsobject.NewStaticObjectInfo(folderName+"/controlled.rmdoc", time.Now(), documentInfo.Size(), true, nil, backend)
	uploaded, err := backend.Put(ctx, documentFile, source)
	closeErr := documentFile.Close()
	if err != nil {
		t.Fatalf("Put controlled rmdoc: %v", err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if uploaded.(*Object).item.ID != documentID {
		t.Fatalf("Put UUID = %q, want %q", uploaded.(*Object).item.ID, documentID)
	}
	entries, err := backend.List(ctx, folderName)
	if err != nil {
		t.Fatalf("list uploaded rmdoc: %v", err)
	}
	assertEntry(t, entries, folderName+"/controlled.rmdoc", false)

	object, err := backend.NewObject(ctx, folderName+"/controlled.rmdoc")
	if err != nil {
		t.Fatalf("find uploaded rmdoc: %v", err)
	}
	if object.(*Object).item.ID != documentID {
		t.Fatalf("uploaded UUID = %q, want %q", object.(*Object).item.ID, documentID)
	}
	if _, err := backend.Move(ctx, object, folderName+"/renamed.rmdoc"); err != nil {
		t.Fatalf("rename rmdoc: %v", err)
	}
	renamed, err := backend.NewObject(ctx, folderName+"/renamed.rmdoc")
	if err != nil {
		t.Fatalf("find renamed rmdoc: %v", err)
	}
	if renamed.(*Object).item.ID != documentID {
		t.Fatalf("rename changed UUID to %q", renamed.(*Object).item.ID)
	}

	archiveName := folderName + "-archive"
	archive, err := client.Mkdir(ctx, "", archiveName)
	if err != nil {
		t.Fatalf("create move target: %v", err)
	}
	defer func() { _ = client.Remove(ctx, archive.ID) }()
	moved, err := backend.Move(ctx, renamed, archiveName+"/renamed.rmdoc")
	if err != nil {
		t.Fatalf("move rmdoc: %v", err)
	}
	if moved.(*Object).item.ID != documentID {
		t.Fatalf("move changed UUID to %q", moved.(*Object).item.ID)
	}
	reader, err := moved.Open(ctx)
	if err != nil {
		t.Fatalf("materialize rmdoc: %v", err)
	}
	_, readErr := io.ReadAll(reader)
	closeErr = reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read materialized rmdoc: read=%v close=%v", readErr, closeErr)
	}
	if err := moved.Remove(ctx); err != nil {
		t.Fatalf("delete test rmdoc: %v", err)
	}
	if err := backend.Rmdir(ctx, archiveName); err != nil {
		t.Fatalf("remove empty move target: %v", err)
	}
}

// integrationClient connects to the rmfakecloud instance the integration
// suite is configured for. Each call mirrors the account afresh into its own
// metadata cache.
func integrationClient(t *testing.T) Client {
	t.Helper()
	host := os.Getenv("REMARKABLE_HOST")
	if host == "" {
		host = os.Getenv("RMAPI_HOST")
	}
	if host == "" {
		host = "http://127.0.0.1:7632"
	}
	configPath := os.Getenv("REMARKABLE_CONFIG")
	if configPath == "" {
		configPath = os.Getenv("RMAPI_CONFIG")
	}
	if configPath == "" && os.Getenv("REMARKABLE_USER_TOKEN") == "" {
		t.Fatal("set REMARKABLE_CONFIG/RMAPI_CONFIG or REMARKABLE_USER_TOKEN")
	}
	client, err := NewConfiguredRMAPIClient(Options{
		Host:            host,
		RefreshInterval: fs.Duration(time.Second),
		Config:          configPath,
		ClientCert:      os.Getenv("REMARKABLE_CLIENT_CERT"),
		ClientKey:       os.Getenv("REMARKABLE_CLIENT_KEY"),
		DeviceToken:     os.Getenv("REMARKABLE_DEVICE_TOKEN"),
		UserToken:       os.Getenv("REMARKABLE_USER_TOKEN"),
	}, filepath.Join(t.TempDir(), "rmapi-metadata"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestRMFakecloudSourceViewSync mirrors a local directory into a uniquely
// named top-level folder with rclone sync and source_view: import, an idle
// re-run, a changed source, and a removed source.
func TestRMFakecloudSourceViewSync(t *testing.T) {
	if os.Getenv("REMARKABLE_INTEGRATION") != "1" {
		t.Skip("set REMARKABLE_INTEGRATION=1 to run against rmfakecloud")
	}
	ctx := context.Background()
	client := integrationClient(t)

	folderName := "rclone-remarkable-mirror-" + time.Now().UTC().Format("20060102T150405.000000000")
	folder, err := client.Mkdir(ctx, "", folderName)
	if err != nil {
		t.Fatalf("create test folder: %v", err)
	}
	defer func() {
		children, err := client.List(ctx, folder.ID)
		if err != nil {
			t.Errorf("list test folder %q: %v", folderName, err)
		}
		for _, child := range children {
			if err := client.Remove(ctx, child.ID); err != nil {
				t.Errorf("remove %q from test folder: %v", child.Name, err)
			}
		}
		if err := client.Remove(ctx, folder.ID); err != nil {
			t.Errorf("remove test folder %q: %v", folderName, err)
		}
	}()

	// A document without a PDF or EPUB payload stands in for a notebook:
	// it has no source counterpart, and the filter keeps sync from deleting it.
	notebookID := uuid.NewString()
	notebookPath := filepath.Join(t.TempDir(), "Journal.rmdoc")
	if err := os.WriteFile(notebookPath, validRMDOC(t, notebookID, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upload(ctx, folder.ID, notebookPath); err != nil {
		t.Fatalf("upload stand-in notebook: %v", err)
	}

	fi, err := filter.NewFilter(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"+ *.{pdf,epub}", "- *"} {
		if err := fi.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	ctx = filter.ReplaceConfig(ctx, fi)

	sourceDir := t.TempDir()
	write := func(name string, content []byte, modTime time.Time) {
		t.Helper()
		filePath := filepath.Join(sourceDir, name)
		if err := os.WriteFile(filePath, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filePath, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	firstTime := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond).Add(345 * time.Millisecond)
	write("Report.pdf", validPDF(), firstTime)
	write("Book.epub", validEPUB(t), firstTime)
	write("todo.txt", []byte("not importable"), firstTime)
	src, err := fs.NewFs(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := newFsView(ctx, "integration-mirror", folderName, client, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}

	run := func(name string) *accounting.StatsInfo {
		t.Helper()
		runCtx := accounting.WithStatsGroup(ctx, fmt.Sprintf("%s/%s/%d", t.Name(), name, time.Now().UnixNano()))
		if err := sync.Sync(runCtx, dst, src, false); err != nil {
			t.Fatalf("%s sync: %v", name, err)
		}
		return accounting.Stats(runCtx)
	}
	// listing reads the folder through a separate client and content cache,
	// so it reflects what the server stores rather than this client's state.
	listing := func() map[string]*Object {
		t.Helper()
		fresh, err := newFsView(ctx, "integration-mirror-check", folderName, integrationClient(t), t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		return listedRemotes(t, fresh, "")
	}
	readAll := func(o *Object) []byte {
		t.Helper()
		reader, err := o.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}

	stats := run("import")
	if stats.GetTransfers() != 2 {
		t.Fatalf("import transferred %d files, want 2", stats.GetTransfers())
	}
	imported := listing()
	if got := sortedKeys(imported); strings.Join(got, ",") != "Book.epub,Journal.rmdoc,Report.pdf" {
		t.Fatalf("after import: %v", got)
	}
	if imported["Journal.rmdoc"].item.ID != notebookID {
		t.Fatalf("Journal.rmdoc UUID = %q, want %q", imported["Journal.rmdoc"].item.ID, notebookID)
	}
	for name, object := range imported {
		if object.item.Kind == ItemDocument && name != "Journal.rmdoc" {
			if dt := object.ModTime(ctx).Sub(firstTime); dt <= -time.Second || dt >= time.Second {
				t.Fatalf("%s modification time %s is not within a second of %s", name, object.ModTime(ctx), firstTime)
			}
		}
	}
	if !bytes.Equal(readAll(imported["Report.pdf"]), validPDF()) || imported["Report.pdf"].Size() != int64(len(validPDF())) {
		t.Fatal("Report.pdf does not read back as the imported PDF")
	}
	if err := operations.Check(ctx, &operations.CheckOpt{Fdst: dst, Fsrc: src, OneWay: true}); err != nil {
		t.Fatalf("check after import: %v", err)
	}

	stats = run("idle")
	if stats.GetTransfers() != 0 || stats.GetDeletes() != 0 {
		t.Fatalf("idle sync: %d transfers, %d deletes", stats.GetTransfers(), stats.GetDeletes())
	}
	idle := listing()
	for name, object := range imported {
		if idle[name] == nil || idle[name].item.ID != object.item.ID || idle[name].item.Version != object.item.Version {
			t.Fatalf("idle sync changed %s: %#v", name, idle[name])
		}
	}

	changed := []byte("%PDF-1.7\n% second edition\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
	changedTime := firstTime.Add(time.Hour)
	write("Report.pdf", changed, changedTime)
	stats = run("change")
	if stats.GetTransfers() != 1 || stats.GetDeletes() != 0 {
		t.Fatalf("change sync: %d transfers, %d deletes", stats.GetTransfers(), stats.GetDeletes())
	}
	afterChange := listing()
	report := afterChange["Report.pdf"]
	if report == nil || report.item.ID != imported["Report.pdf"].item.ID {
		t.Fatalf("Report.pdf after change = %#v, want UUID %q", report, imported["Report.pdf"].item.ID)
	}
	if !bytes.Equal(readAll(report), changed) || report.Size() != int64(len(changed)) {
		t.Fatal("Report.pdf does not read back as the changed PDF")
	}
	if report.item.Version <= imported["Report.pdf"].item.Version {
		t.Fatalf("Report.pdf version %d did not advance past %d", report.item.Version, imported["Report.pdf"].item.Version)
	}
	if dt := report.ModTime(ctx).Sub(changedTime); dt <= -time.Second || dt >= time.Second {
		t.Fatalf("Report.pdf modification time %s is not within a second of %s", report.ModTime(ctx), changedTime)
	}
	if stats = run("idle after change"); stats.GetTransfers() != 0 || stats.GetDeletes() != 0 {
		t.Fatalf("idle sync after change: %d transfers, %d deletes", stats.GetTransfers(), stats.GetDeletes())
	}

	if err := os.Remove(filepath.Join(sourceDir, "Book.epub")); err != nil {
		t.Fatal(err)
	}
	stats = run("delete")
	if stats.GetDeletes() != 1 || stats.GetTransfers() != 0 {
		t.Fatalf("delete sync: %d deletes, %d transfers", stats.GetDeletes(), stats.GetTransfers())
	}
	// A fresh mirror, because rmapi keeps a deleted node findable by UUID in
	// the tree it deleted it from until that tree is refreshed.
	if _, err := integrationClient(t).Get(ctx, imported["Book.epub"].item.ID); !errors.Is(err, errItemNotFound) {
		t.Fatalf("Book.epub UUID %q still exists: %v", imported["Book.epub"].item.ID, err)
	}
	if got := sortedKeys(listing()); strings.Join(got, ",") != "Journal.rmdoc,Report.pdf" {
		t.Fatalf("after delete: %v", got)
	}
}

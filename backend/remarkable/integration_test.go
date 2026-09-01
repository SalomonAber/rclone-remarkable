package remarkable

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	client, err := newConfiguredRMAPIClient(Options{
		Host:        host,
		Config:      configPath,
		DeviceToken: os.Getenv("REMARKABLE_DEVICE_TOKEN"),
		UserToken:   os.Getenv("REMARKABLE_USER_TOKEN"),
	})
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

	rmapiClient, ok := client.(*rmapiClient)
	if !ok {
		t.Fatal("configured client is not rmapi-backed")
	}
	documentPath := filepath.Join(t.TempDir(), "controlled.rmdoc")
	if err := os.WriteFile(documentPath, rmdocArchive(t, "controlled"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := rmapiClient.api.UploadDocument(folder.ID, documentPath, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("upload controlled rmdoc: %v", err)
	}
	if _, _, err := rmapiClient.api.Refresh(); err != nil {
		t.Fatalf("refresh after upload: %v", err)
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
	if object.(*Object).item.ID != document.ID {
		t.Fatalf("uploaded UUID = %q, want %q", object.(*Object).item.ID, document.ID)
	}
	if _, err := backend.Move(ctx, object, folderName+"/renamed.rmdoc"); err != nil {
		t.Fatalf("rename rmdoc: %v", err)
	}
	renamed, err := backend.NewObject(ctx, folderName+"/renamed.rmdoc")
	if err != nil {
		t.Fatalf("find renamed rmdoc: %v", err)
	}
	if renamed.(*Object).item.ID != document.ID {
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
	if moved.(*Object).item.ID != document.ID {
		t.Fatalf("move changed UUID to %q", moved.(*Object).item.ID)
	}
	reader, err := moved.Open(ctx)
	if err != nil {
		t.Fatalf("materialize rmdoc: %v", err)
	}
	_, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
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

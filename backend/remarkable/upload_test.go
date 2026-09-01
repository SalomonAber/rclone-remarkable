package remarkable

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

const uploadDocumentID = "2a9b9220-45d8-49c3-8393-2a1fd8a5a6f7"

func TestPutCreatesValidatedRMDOC(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{
		"work": {ID: "work", Name: "Work", Kind: ItemDirectory},
	}}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := validRMDOC(t, uploadDocumentID, 1024)
	src := object.NewStaticObjectInfo("Work/Meeting Notes 漢字.rmdoc", time.Now(), int64(len(content)), true, nil, backend)
	uploaded, err := backend.Put(ctx, bytes.NewReader(content), src)
	if err != nil {
		t.Fatal(err)
	}
	got := uploaded.(*Object)
	if got.item.ID != uploadDocumentID || got.item.Name != "Meeting Notes 漢字" || got.item.ParentID != "work" {
		t.Fatalf("uploaded item = %#v", got.item)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.uploads) != 1 || filepath.Base(client.uploads[0].SourcePath) != "Meeting Notes 漢字.rmdoc" {
		t.Fatalf("upload calls = %#v", client.uploads)
	}
}

func TestPutValidatesBeforeUpload(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "not a zip", content: []byte("not an rmdoc")},
		{name: "missing metadata", content: rmdocWithoutMetadata(t, uploadDocumentID)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := &fakeClient{items: map[string]Item{}}
			cacheDir := t.TempDir()
			backend, err := newFs(ctx, "test", "", client, cacheDir)
			if err != nil {
				t.Fatal(err)
			}
			src := object.NewStaticObjectInfo("Invalid.rmdoc", time.Now(), int64(len(test.content)), true, nil, backend)
			uploaded, err := backend.Put(ctx, bytes.NewReader(test.content), src)
			if err == nil || uploaded != nil {
				t.Fatalf("Put = (%v, %v), want validation error", uploaded, err)
			}
			if len(client.uploads) != 0 {
				t.Fatalf("invalid archive reached client: %#v", client.uploads)
			}
			assertUploadTempEmpty(t, backend.uploadTempDir)
		})
	}
}

func TestPutCollisionAndOverwriteAreExplicit(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{
		items: map[string]Item{
			"existing": {ID: "existing", Name: "Existing", Kind: ItemDocument, Version: 1},
		},
		contents: map[string][]byte{"existing": rmdocArchive(t, "existing")},
	}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := validRMDOC(t, uploadDocumentID, 128)
	src := object.NewStaticObjectInfo("Existing.rmdoc", time.Now(), int64(len(content)), true, nil, backend)
	if uploaded, err := backend.Put(ctx, bytes.NewReader(content), src); !errors.Is(err, errDestinationExists) || uploaded != nil {
		t.Fatalf("collision Put = (%v, %v)", uploaded, err)
	}
	existing, err := backend.NewObject(ctx, "Existing.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if err := existing.Update(ctx, bytes.NewReader(content), src); !errors.Is(err, fs.ErrorNotImplemented) {
		t.Fatalf("overwrite error = %v", err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("collision/overwrite invoked upload: %#v", client.uploads)
	}
}

func TestPutRejectsExistingUUIDAtDifferentPath(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{
		uploadDocumentID: {ID: uploadDocumentID, Name: "Original", Kind: ItemDocument, Version: 1},
	}}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := validRMDOC(t, uploadDocumentID, 128)
	src := object.NewStaticObjectInfo("Different Name.rmdoc", time.Now(), int64(len(content)), true, nil, backend)
	if uploaded, err := backend.Put(ctx, bytes.NewReader(content), src); !errors.Is(err, errDestinationExists) || uploaded != nil {
		t.Fatalf("UUID collision Put = (%v, %v)", uploaded, err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("UUID collision invoked upload: %#v", client.uploads)
	}
}

func TestPutLargeFileUsesDiskStaging(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{}}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "Large.rmdoc")
	writeRMDOC(t, sourcePath, uploadDocumentID, 32<<20)
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		t.Fatal(err)
	}
	src := object.NewStaticObjectInfo("Large File.rmdoc", time.Now(), info.Size(), true, nil, backend)
	uploaded, err := backend.Put(ctx, source, src)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Size() != -1 {
		t.Fatalf("transformed upload size = %d, want unknown", uploaded.Size())
	}
	assertUploadTempEmpty(t, backend.uploadTempDir)
}

func TestPutInterruptedWriteCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &fakeClient{items: map[string]Item{}}
	backend, err := newFs(context.Background(), "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := object.NewStaticObjectInfo("Interrupted.rmdoc", time.Now(), -1, true, nil, backend)
	if uploaded, err := backend.Put(ctx, bytes.NewReader(validRMDOC(t, uploadDocumentID, 128)), src); !errors.Is(err, context.Canceled) || uploaded != nil {
		t.Fatalf("interrupted Put = (%v, %v)", uploaded, err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("interrupted write invoked upload: %#v", client.uploads)
	}
	assertUploadTempEmpty(t, backend.uploadTempDir)
}

func TestPutRetryAfterRemoteError(t *testing.T) {
	ctx := context.Background()
	transient := errors.New("transient upload failure")
	client := &fakeClient{items: map[string]Item{}, uploadErrors: []error{transient, nil}}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := validRMDOC(t, uploadDocumentID, 128)
	src := object.NewStaticObjectInfo("Retry.rmdoc", time.Now(), int64(len(content)), true, nil, backend)
	if uploaded, err := backend.Put(ctx, bytes.NewReader(content), src); !errors.Is(err, transient) || uploaded != nil {
		t.Fatalf("first Put = (%v, %v)", uploaded, err)
	}
	if _, err := client.Get(ctx, uploadDocumentID); err == nil {
		t.Fatal("failed upload became visible")
	}
	uploaded, err := backend.Put(ctx, bytes.NewReader(content), src)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.(*Object).item.ID != uploadDocumentID || len(client.uploads) != 2 {
		t.Fatalf("retry result = %#v, calls = %#v", uploaded, client.uploads)
	}
	assertUploadTempEmpty(t, backend.uploadTempDir)
}

func validRMDOC(t *testing.T, id string, payloadSize int64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writeRMDOCTo(t, &buffer, id, payloadSize)
	return buffer.Bytes()
}

func writeRMDOC(t *testing.T, filePath, id string, payloadSize int64) {
	t.Helper()
	file, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}
	writeRMDOCTo(t, file, id, payloadSize)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeRMDOCTo(t *testing.T, dst io.Writer, id string, payloadSize int64) {
	t.Helper()
	writer := zip.NewWriter(dst)
	metadata, err := writer.Create(id + ".metadata")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.Write([]byte(`{"visibleName":"source","type":"DocumentType"}`)); err != nil {
		t.Fatal(err)
	}
	content, err := writer.Create(id + ".content")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := content.Write([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	payload, err := writer.CreateHeader(&zip.FileHeader{Name: id + ".payload", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(payload, zeroReader{}, payloadSize); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func rmdocWithoutMetadata(t *testing.T, id string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	content, err := writer.Create(id + ".content")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = content.Write([]byte(`{}`))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func assertUploadTempEmpty(t *testing.T, tempRoot string) {
	t.Helper()
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload temp files remain: %v", entries)
	}
}

package remarkable

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
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

func TestPutImportsNativeDocuments(t *testing.T) {
	tests := []struct {
		name      string
		remote    string
		content   []byte
		wantLocal string
		wantExt   string
	}{
		{name: "PDF", remote: "Work/Quarterly Report.PDF", content: validPDF(), wantLocal: "Work/Quarterly Report.pdf.rmdoc", wantExt: ".pdf"},
		{name: "EPUB", remote: "Work/An Excellent Book.epub", content: validEPUB(t), wantLocal: "Work/An Excellent Book.epub.rmdoc", wantExt: ".epub"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := &fakeClient{items: map[string]Item{
				"work": {ID: "work", Name: "Work", Kind: ItemDirectory},
			}}
			backend, err := newFs(ctx, "test", "", client, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			src := object.NewStaticObjectInfo(test.remote, time.Now(), int64(len(test.content)), true, nil, backend)
			uploaded, err := backend.Put(ctx, bytes.NewReader(test.content), src)
			if err != nil {
				t.Fatal(err)
			}
			got := uploaded.(*Object)
			if got.Remote() != test.wantLocal || got.item.Name != strings.TrimSuffix(path.Base(test.wantLocal), ".rmdoc") || got.item.ParentID != "work" {
				t.Fatalf("uploaded object = %#v, remote = %q", got.item, got.Remote())
			}
			if got.Size() != -1 {
				t.Fatalf("imported object size = %d, want unknown", got.Size())
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if len(client.uploads) != 1 || strings.ToLower(filepath.Ext(client.uploads[0].SourcePath)) != test.wantExt {
				t.Fatalf("upload calls = %#v", client.uploads)
			}
		})
	}
}

func TestPutRejectsInvalidNativeDocumentsBeforeUpload(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		content []byte
	}{
		{name: "invalid PDF", remote: "Broken.pdf", content: []byte("not a pdf")},
		{name: "invalid EPUB", remote: "Broken.epub", content: invalidEPUB(t)},
		{name: "unsupported extension", remote: "Notes.txt", content: []byte("notes")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := &fakeClient{items: map[string]Item{}}
			backend, err := newFs(ctx, "test", "", client, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			src := object.NewStaticObjectInfo(test.remote, time.Now(), int64(len(test.content)), true, nil, backend)
			if uploaded, err := backend.Put(ctx, bytes.NewReader(test.content), src); err == nil || uploaded != nil {
				t.Fatalf("Put = (%v, %v), want validation error", uploaded, err)
			}
			if len(client.uploads) != 0 {
				t.Fatalf("invalid import reached client: %#v", client.uploads)
			}
			assertUploadTempEmpty(t, backend.uploadTempDir)
		})
	}
}

func TestNativeImportChecksCanonicalRMDOCNameForCollision(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{
		"existing": {ID: "existing", Name: "Report.pdf", Kind: ItemDocument, Version: 1},
	}}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := validPDF()
	src := object.NewStaticObjectInfo("Report.pdf", time.Now(), int64(len(content)), true, nil, backend)
	if uploaded, err := backend.Put(ctx, bytes.NewReader(content), src); !errors.Is(err, errDestinationExists) || uploaded != nil {
		t.Fatalf("collision Put = (%v, %v)", uploaded, err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("collision invoked upload: %#v", client.uploads)
	}
}

func TestNativeImportsWithSameBasenameRetainDistinctFormats(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{}}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imports := []struct {
		remote  string
		content []byte
	}{
		{remote: "File.pdf", content: validPDF()},
		{remote: "File.epub", content: validEPUB(t)},
	}
	for _, source := range imports {
		src := object.NewStaticObjectInfo(source.remote, time.Now(), int64(len(source.content)), true, nil, backend)
		if _, err := backend.Put(ctx, bytes.NewReader(source.content), src); err != nil {
			t.Fatalf("import %q: %v", source.remote, err)
		}
	}
	entries, err := backend.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	assertEntry(t, entries, "File.pdf.rmdoc", false)
	assertEntry(t, entries, "File.epub.rmdoc", false)
	if len(entries) != 2 {
		t.Fatalf("root entries = %#v, want exactly the two distinct imports", entries)
	}
}

func TestVFSDragAndDropCanonicalizesNativeDocumentAfterClose(t *testing.T) {
	tests := []struct {
		name      string
		fileName  string
		canonical string
		content   []byte
	}{
		{name: "PDF", fileName: "Dragged Report.pdf", canonical: "Dragged Report.pdf.rmdoc", content: validPDF()},
		{name: "EPUB", fileName: "Dragged Book.epub", canonical: "Dragged Book.epub.rmdoc", content: validEPUB(t)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := &fakeClient{items: map[string]Item{
				"work": {ID: "work", Name: "Work", Kind: ItemDirectory},
			}}
			backend, err := newFs(ctx, "vfs-import-"+strings.ToLower(test.name), "", client, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			oldCacheDir := config.GetCacheDir()
			if err := config.SetCacheDir(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := config.SetCacheDir(oldCacheDir); err != nil {
					t.Errorf("restore cache directory: %v", err)
				}
			})

			options := vfscommon.Opt
			options.CacheMode = vfscommon.CacheModeFull
			options.WriteBack = 0
			options.PollInterval = 0
			mounted := vfs.New(ctx, backend, &options)
			t.Cleanup(func() {
				mounted.WaitForWriters(5 * time.Second)
				if err := mounted.CleanUp(); err != nil {
					t.Errorf("clean up VFS: %v", err)
				}
				mounted.Shutdown()
			})

			remote := path.Join("Work", test.fileName)
			handle, err := mounted.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			split := len(test.content) / 2
			if _, err := handle.Write(test.content[:split]); err != nil {
				t.Fatal(err)
			}
			assertVFSNames(t, mounted, "Work", test.fileName)
			if len(client.uploads) != 0 {
				t.Fatalf("upload started before source close: %#v", client.uploads)
			}
			if _, err := handle.Write(test.content[split:]); err != nil {
				t.Fatal(err)
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}

			assertVFSNames(t, mounted, "Work", test.canonical)
			if len(client.uploads) != 1 {
				t.Fatalf("upload calls = %#v", client.uploads)
			}
		})
	}
}

func assertVFSNames(t *testing.T, mounted *vfs.VFS, dir string, want ...string) {
	t.Helper()
	entries, err := mounted.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(entries))
	for index, entry := range entries {
		got[index] = entry.Name()
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("VFS entries in %q = %q, want %q", dir, got, want)
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

func validPDF() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
}

func validEPUB(t *testing.T) []byte {
	t.Helper()
	return epubArchive(t, true)
}

func invalidEPUB(t *testing.T) []byte {
	t.Helper()
	return epubArchive(t, false)
}

func epubArchive(t *testing.T, includeContainer bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	mimetypeHeader := &zip.FileHeader{Name: "mimetype", Method: zip.Store}
	mimetype, err := writer.CreateHeader(mimetypeHeader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mimetype.Write([]byte("application/epub+zip")); err != nil {
		t.Fatal(err)
	}
	if includeContainer {
		container, err := writer.Create("META-INF/container.xml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := container.Write([]byte(`<?xml version="1.0"?><container><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`)); err != nil {
			t.Fatal(err)
		}
	}
	packageFile, err := writer.Create("OEBPS/content.opf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := packageFile.Write([]byte(`<?xml version="1.0"?><package/>`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
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
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload temp files remain: %v", entries)
	}
}

package remarkable

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
)

func TestReadOnlyTreeMapping(t *testing.T) {
	ctx := context.Background()
	client := treeClient(t)
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	root, err := backend.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	assertEntry(t, root, "Work", true)
	assertEntry(t, root, "Scratchpad.rmdoc", false)
	assertEntry(t, root, "Résumé 漢字.rmdoc", false)

	work, err := backend.List(ctx, "Work")
	if err != nil {
		t.Fatal(err)
	}
	assertEntry(t, work, "Work/Meeting Notes.rmdoc", false)
	assertEntry(t, work, "Work/Design.rmdoc", false)

	object, err := backend.NewObject(ctx, "Work/Meeting Notes.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if object.Remote() != "Work/Meeting Notes.rmdoc" {
		t.Fatalf("remote = %q", object.Remote())
	}
	if object.ModTime(ctx) != client.items["meeting"].ModTime {
		t.Fatalf("mod time = %s", object.ModTime(ctx))
	}
	if object.Size() <= 0 {
		t.Fatalf("size = %d", object.Size())
	}
	reader, err := object.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("Open returned invalid rmdoc archive: %v", err)
	}
	if len(zipReader.File) == 0 {
		t.Fatal("rmdoc archive is empty")
	}
}

func TestRootResolution(t *testing.T) {
	ctx := context.Background()
	backend, err := newFs(ctx, "test", "Work", treeClient(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if backend.rootID != "work" {
		t.Fatalf("root UUID = %q", backend.rootID)
	}
	entries, err := backend.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	assertEntry(t, entries, "Meeting Notes.rmdoc", false)
	assertEntry(t, entries, "Design.rmdoc", false)
}

func TestRMDOCSuffixHandling(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{
		items: map[string]Item{
			"document": {ID: "document", Name: "Draft.rmdoc", Kind: ItemDocument, Version: 1},
		},
		contents: map[string][]byte{"document": rmdocArchive(t, "document")},
	}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := backend.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	assertEntry(t, entries, "Draft.rmdoc.rmdoc", false)
	if _, err := backend.NewObject(ctx, "Draft.rmdoc"); err != fs.ErrorObjectNotFound {
		t.Fatalf("unsuffixed remote error = %v", err)
	}
}

func TestDuplicateVisibleNames(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{
		"first":  {ID: "first", Name: "Same", Kind: ItemDocument},
		"second": {ID: "second", Name: "Same", Kind: ItemDocument},
	}}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.List(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "duplicate visible name") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestUUIDIdentityIndependentOfPath(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{
		items: map[string]Item{
			"work": {ID: "work", Name: "Work", Kind: ItemDirectory},
			"doc":  {ID: "doc", Name: "Foo", ParentID: "work", Kind: ItemDocument, Version: 3},
		},
		contents: map[string][]byte{"doc": rmdocArchive(t, "doc")},
	}
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before, err := backend.NewObject(ctx, "Work/Foo.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	client.items = map[string]Item{
		"archive": {ID: "archive", Name: "Archive", Kind: ItemDirectory},
		"doc":     {ID: "doc", Name: "Bar", ParentID: "archive", Kind: ItemDocument, Version: 3},
	}
	after, err := backend.NewObject(ctx, "Archive/Bar.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if before.(*Object).item.ID != after.(*Object).item.ID {
		t.Fatalf("UUID changed from %q to %q", before.(*Object).item.ID, after.(*Object).item.ID)
	}
	if before.Remote() == after.Remote() {
		t.Fatalf("presentation path did not change: %q", before.Remote())
	}
}

func TestVersionSpecificCache(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{
		contents: map[string][]byte{"doc": []byte("version one")},
	}
	cacheDir := t.TempDir()
	cache := newContentCache(cacheDir, client)
	versionOne := Item{ID: "doc", Version: 1}
	pathOne, _, err := cache.materialize(ctx, versionOne)
	if err != nil {
		t.Fatal(err)
	}
	wantOne := filepath.Join(cacheDir, "doc", "1.rmdoc")
	if pathOne != wantOne {
		t.Fatalf("cache path = %q, want %q", pathOne, wantOne)
	}
	if _, _, err := cache.materialize(ctx, versionOne); err != nil {
		t.Fatal(err)
	}
	if calls := client.downloadCount("doc"); calls != 1 {
		t.Fatalf("cache hit downloads = %d, want 1", calls)
	}

	client.contents["doc"] = []byte("version two")
	pathTwo, _, err := cache.materialize(ctx, Item{ID: "doc", Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if pathTwo == pathOne || pathTwo != filepath.Join(cacheDir, "doc", "2.rmdoc") {
		t.Fatalf("version two cache path = %q", pathTwo)
	}
	if calls := client.downloadCount("doc"); calls != 2 {
		t.Fatalf("version change downloads = %d, want 2", calls)
	}
}

func TestConcurrentOpensDeduplicateMaterialization(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{
		contents:        map[string][]byte{"doc": []byte("concurrent content")},
		downloadStarted: make(chan struct{}, 1),
		releaseDownload: make(chan struct{}),
	}
	cacheDir := t.TempDir()
	cache := newContentCache(cacheDir, client)
	backend := &Fs{cache: cache}
	object := &Object{fs: backend, item: Item{ID: "doc", Version: 9}, size: 18}

	const readers = 8
	errors := make(chan error, readers)
	var waitGroup sync.WaitGroup
	for range readers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			reader, err := object.Open(ctx)
			if err == nil {
				_, err = io.ReadAll(reader)
				if closeErr := reader.Close(); err == nil {
					err = closeErr
				}
			}
			errors <- err
		}()
	}
	<-client.downloadStarted
	finalPath, err := cache.path("doc", 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("final cache path visible during download: %v", err)
	}
	close(client.releaseDownload)
	waitGroup.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := client.downloadCount("doc"); calls != 1 {
		t.Fatalf("concurrent downloads = %d, want 1", calls)
	}
}

func TestCanceledMaterializationRemovesPartialFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cacheDir := t.TempDir()
	client := &fakeClient{
		contents:        map[string][]byte{"doc": []byte("partial")},
		releaseDownload: make(chan struct{}),
	}
	cache := newContentCache(cacheDir, client)
	if _, _, err := cache.materialize(ctx, Item{ID: "doc", Version: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("materialize error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(cacheDir, "doc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial cache files remain: %v", entries)
	}
}

func TestRangeReadFromCache(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{contents: map[string][]byte{"doc": []byte("0123456789")}}
	backend := &Fs{cache: newContentCache(t.TempDir(), client)}
	object := &Object{fs: backend, item: Item{ID: "doc", Version: 1}, size: 10}

	reader, err := object.Open(ctx, &fs.RangeOption{Start: 2, End: 5})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "2345" {
		t.Fatalf("range content = %q", content)
	}

	reader, err = object.Open(ctx, &fs.SeekOption{Offset: 7})
	if err != nil {
		t.Fatal(err)
	}
	content, err = io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(content) != "789" {
		t.Fatalf("seek content = %q, error = %v", content, err)
	}
}

func treeClient(t *testing.T) *fakeClient {
	t.Helper()
	modTime := time.Date(2026, time.September, 1, 9, 0, 0, 0, time.UTC)
	items := map[string]Item{
		"work":       {ID: "work", Name: "Work", Kind: ItemDirectory, ModTime: modTime},
		"scratchpad": {ID: "scratchpad", Name: "Scratchpad", Kind: ItemDocument, Version: 1, ModTime: modTime},
		"unicode":    {ID: "unicode", Name: "Résumé 漢字", Kind: ItemDocument, Version: 1, ModTime: modTime},
		"meeting":    {ID: "meeting", Name: "Meeting Notes", ParentID: "work", Kind: ItemDocument, Version: 4, ModTime: modTime},
		"design":     {ID: "design", Name: "Design", ParentID: "work", Kind: ItemDocument, Version: 2, ModTime: modTime},
	}
	contents := make(map[string][]byte)
	for id, item := range items {
		if item.Kind == ItemDocument {
			contents[id] = rmdocArchive(t, id)
		}
	}
	return &fakeClient{items: items, contents: contents}
}

func rmdocArchive(t *testing.T, id string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create(id + ".metadata")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`{"visibleName":"test"}`)); err != nil {
		t.Fatal(err)
	}
	entry, err = writer.Create(id + ".content")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func assertEntry(t *testing.T, entries fs.DirEntries, remote string, directory bool) {
	t.Helper()
	for _, entry := range entries {
		if entry.Remote() != remote {
			continue
		}
		_, isDirectory := entry.(fs.Directory)
		if isDirectory != directory {
			t.Fatalf("entry %q directory = %t", remote, isDirectory)
		}
		return
	}
	t.Fatalf("entry %q not found in %v", remote, entries)
}

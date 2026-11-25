package remarkable

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/sync"
)

var sourceViewModTime = time.Date(2026, time.November, 20, 10, 30, 15, 0, time.UTC)

func payloadArchive(t *testing.T, id, fileType string, payload []byte) []byte {
	t.Helper()
	content, err := fakeRMDOC(id, id+"."+fileType, payload)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func notebookArchive(t *testing.T, id string) []byte {
	t.Helper()
	content, err := fakeRMDOC(id, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// sourceViewClient holds a Personal collection with an imported PDF and EPUB,
// a notebook, and a PDF whose visible name lacks its extension.
func sourceViewClient(t *testing.T) *fakeClient {
	t.Helper()
	document := func(id, name string) Item {
		return Item{ID: id, Name: name, ParentID: "personal", Kind: ItemDocument, Version: 3, ModTime: sourceViewModTime}
	}
	return &fakeClient{
		items: map[string]Item{
			"personal": {ID: "personal", Name: "Personal", Kind: ItemDirectory},
			"report":   document("report", "Report.pdf"),
			"book":     document("book", "Book.epub"),
			"notes":    document("notes", "Notes"),
			"scan":     document("scan", "Scan"),
		},
		contents: map[string][]byte{
			"report": payloadArchive(t, "report", "pdf", validPDF()),
			"book":   payloadArchive(t, "book", "epub", validEPUB(t)),
			"notes":  notebookArchive(t, "notes"),
			"scan":   payloadArchive(t, "scan", "pdf", validPDF()),
		},
	}
}

func newSourceViewFs(t *testing.T, client *fakeClient) *Fs {
	t.Helper()
	backend, err := newFsView(context.Background(), "test", "", client, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func listedRemotes(t *testing.T, backend *Fs, dir string) map[string]*Object {
	t.Helper()
	entries, err := backend.List(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	objects := make(map[string]*Object)
	for _, entry := range entries {
		if object, ok := entry.(*Object); ok {
			objects[entry.Remote()] = object
		}
	}
	return objects
}

func sortedKeys(objects map[string]*Object) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func TestSourceViewName(t *testing.T) {
	tests := []struct {
		name     string
		item     Item
		fileType string
		want     string
	}{
		{name: "imported PDF", item: Item{Name: "Report.pdf"}, fileType: "pdf", want: "Report.pdf"},
		{name: "imported EPUB", item: Item{Name: "Book.epub"}, fileType: "epub", want: "Book.epub"},
		{name: "extension case is kept", item: Item{Name: "Scan.PDF"}, fileType: "pdf", want: "Scan.PDF"},
		{name: "PDF without extension", item: Item{Name: "Scan"}, fileType: "pdf", want: "Scan.rmdoc"},
		{name: "notebook with a PDF name", item: Item{Name: "Report.pdf"}, fileType: "", want: "Report.pdf.rmdoc"},
		{name: "EPUB with a PDF name", item: Item{Name: "Book.pdf"}, fileType: "epub", want: "Book.pdf.rmdoc"},
		{name: "bare extension", item: Item{Name: ".pdf"}, fileType: "pdf", want: ".pdf.rmdoc"},
		{name: "collection", item: Item{Name: "Report.pdf", Kind: ItemDirectory}, fileType: "pdf", want: "Report.pdf"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sourceViewName(test.item, test.fileType); got != test.want {
				t.Fatalf("sourceViewName(%q, %q) = %q, want %q", test.item.Name, test.fileType, got, test.want)
			}
			wantRMDOC := test.item.Name + ".rmdoc"
			if test.item.Kind == ItemDirectory {
				wantRMDOC = test.item.Name
			}
			if got := localName(test.item); got != wantRMDOC {
				t.Fatalf("localName(%q) = %q, want %q", test.item.Name, got, wantRMDOC)
			}
		})
	}
}

func TestListNamesInBothViews(t *testing.T) {
	ctx := context.Background()
	client := sourceViewClient(t)
	rmdocView, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedKeys(listedRemotes(t, rmdocView, "Personal")), []string{
		"Personal/Book.epub.rmdoc", "Personal/Notes.rmdoc", "Personal/Report.pdf.rmdoc", "Personal/Scan.rmdoc",
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rmdoc view = %v, want %v", got, want)
	}

	sourceView := newSourceViewFs(t, client)
	objects := listedRemotes(t, sourceView, "Personal")
	if got, want := sortedKeys(objects), []string{
		"Personal/Book.epub", "Personal/Notes.rmdoc", "Personal/Report.pdf", "Personal/Scan.rmdoc",
	}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("source view = %v, want %v", got, want)
	}
	report := objects["Personal/Report.pdf"]
	if report.item.ID != "report" || report.Size() != int64(len(validPDF())) || !report.ModTime(ctx).Equal(sourceViewModTime) {
		t.Fatalf("Report.pdf = UUID %q size %d time %s", report.item.ID, report.Size(), report.ModTime(ctx))
	}
	if objects["Personal/Book.epub"].Size() != int64(len(validEPUB(t))) {
		t.Fatalf("Book.epub size = %d", objects["Personal/Book.epub"].Size())
	}
	if notes := objects["Personal/Notes.rmdoc"]; notes.Size() != int64(len(client.contents["notes"])) {
		t.Fatalf("Notes.rmdoc size = %d, want the archive size %d", notes.Size(), len(client.contents["notes"]))
	}
	if scan := objects["Personal/Scan.rmdoc"]; scan.Size() != int64(len(client.contents["scan"])) {
		t.Fatalf("Scan.rmdoc size = %d, want the archive size %d", scan.Size(), len(client.contents["scan"]))
	}
}

func TestSourceViewSiblingCollisions(t *testing.T) {
	ctx := context.Background()
	t.Run("notebook named like an imported PDF", func(t *testing.T) {
		client := sourceViewClient(t)
		client.items["notebook"] = Item{ID: "notebook", Name: "Report.pdf", ParentID: "personal", Kind: ItemDocument, Version: 1}
		client.contents["notebook"] = notebookArchive(t, "notebook")

		rmdocView, err := newFs(ctx, "test", "", client, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rmdocView.List(ctx, "Personal"); err == nil || !strings.Contains(err.Error(), "duplicate visible name") {
			t.Fatalf("rmdoc view List error = %v, want a duplicate name", err)
		}

		sourceView := newSourceViewFs(t, client)
		objects := listedRemotes(t, sourceView, "Personal")
		if objects["Personal/Report.pdf"].item.ID != "report" || objects["Personal/Report.pdf.rmdoc"].item.ID != "notebook" {
			t.Fatalf("source view = %v", sortedKeys(objects))
		}
		for remote, want := range map[string]string{"Personal/Report.pdf": "report", "Personal/Report.pdf.rmdoc": "notebook"} {
			found, err := sourceView.NewObject(ctx, remote)
			if err != nil {
				t.Fatal(err)
			}
			if found.(*Object).item.ID != want {
				t.Fatalf("NewObject(%q) = UUID %q, want %q", remote, found.(*Object).item.ID, want)
			}
		}
	})
	t.Run("collection named like an imported PDF", func(t *testing.T) {
		client := sourceViewClient(t)
		client.items["folder"] = Item{ID: "folder", Name: "Report.pdf", ParentID: "personal", Kind: ItemDirectory}
		sourceView := newSourceViewFs(t, client)
		if _, err := sourceView.List(ctx, "Personal"); err == nil || !strings.Contains(err.Error(), "duplicate visible name") {
			t.Fatalf("List error = %v, want a duplicate name", err)
		}
		if _, err := sourceView.NewObject(ctx, "Personal/Report.pdf"); err == nil || !strings.Contains(err.Error(), "duplicate visible name") {
			t.Fatalf("NewObject error = %v, want a duplicate name", err)
		}
	})
	t.Run("two imports of one name", func(t *testing.T) {
		client := sourceViewClient(t)
		client.items["again"] = Item{ID: "again", Name: "Report.pdf", ParentID: "personal", Kind: ItemDocument, Version: 1}
		client.contents["again"] = payloadArchive(t, "again", "pdf", validPDF())
		sourceView := newSourceViewFs(t, client)
		if _, err := sourceView.List(ctx, "Personal"); err == nil || !strings.Contains(err.Error(), "duplicate visible name") {
			t.Fatalf("List error = %v, want a duplicate name", err)
		}
	})
}

func TestSourceViewResolveMaterializesOnlyCandidates(t *testing.T) {
	ctx := context.Background()
	client := sourceViewClient(t)
	sourceView := newSourceViewFs(t, client)
	if _, err := sourceView.NewObject(ctx, "Personal/Book.epub"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"report", "notes", "scan"} {
		if got := client.downloadCount(id); got != 0 {
			t.Fatalf("resolving Book.epub downloaded %q %d times", id, got)
		}
	}
}

func TestSourceViewOpenAndHashReturnPayload(t *testing.T) {
	ctx := context.Background()
	client := sourceViewClient(t)
	sourceView := newSourceViewFs(t, client)
	report, err := sourceView.NewObject(ctx, "Personal/Report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	want := validPDF()
	reader, err := report.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Open = %q, %v; want the PDF", got, err)
	}
	reader, err = report.Open(ctx, &fs.RangeOption{Start: 2, End: 6})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(reader)
	_ = reader.Close()
	if !bytes.Equal(got, want[2:7]) {
		t.Fatalf("range read = %q, want %q", got, want[2:7])
	}
	sum, err := report.Hash(ctx, hash.MD5)
	if err != nil || sum != md5Hex(want) {
		t.Fatalf("MD5 = %q, %v; want %q", sum, err, md5Hex(want))
	}
	notes, err := sourceView.NewObject(ctx, "Personal/Notes.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if sum, err := notes.Hash(ctx, hash.MD5); sum != "" || err != nil {
		t.Fatalf("notebook MD5 = %q, %v; want none", sum, err)
	}
	if got := client.downloadCount("report"); got != 1 {
		t.Fatalf("Report.pdf downloads = %d, want one shared by list, open, and hash", got)
	}
}

func TestSourceViewFeaturesAndModTime(t *testing.T) {
	ctx := context.Background()
	client := sourceViewClient(t)
	rmdocView, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rmdocView.Precision() != time.Millisecond || rmdocView.Hashes() != hash.Set(hash.None) {
		t.Fatalf("rmdoc view precision %s hashes %v", rmdocView.Precision(), rmdocView.Hashes())
	}
	sourceView := newSourceViewFs(t, client)
	if sourceView.Precision() != time.Second || sourceView.Hashes() != hash.Set(hash.MD5) {
		t.Fatalf("source view precision %s hashes %v", sourceView.Precision(), sourceView.Hashes())
	}

	archived, err := rmdocView.NewObject(ctx, "Personal/Report.pdf.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if err := archived.SetModTime(ctx, time.Now()); !errors.Is(err, fs.ErrorCantSetModTime) {
		t.Fatalf("rmdoc view SetModTime = %v", err)
	}
	if _, err := archived.Hash(ctx, hash.MD5); !errors.Is(err, hash.ErrUnsupported) {
		t.Fatalf("rmdoc view Hash = %v", err)
	}
	notes, err := sourceView.NewObject(ctx, "Personal/Notes.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if err := notes.SetModTime(ctx, time.Now()); !errors.Is(err, fs.ErrorCantSetModTime) {
		t.Fatalf("notebook SetModTime = %v", err)
	}

	report, err := sourceView.NewObject(ctx, "Personal/Report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := report.Open(ctx); err != nil {
		t.Fatal(err)
	}
	downloads := client.downloadCount("report")
	modTime := time.Date(2026, time.October, 1, 8, 0, 0, 750_000_000, time.UTC)
	if err := report.SetModTime(ctx, modTime); err != nil {
		t.Fatal(err)
	}
	if len(client.modTimes) != 1 || client.modTimes[0] != (modTimeCall{ID: "report", ModTime: modTime}) {
		t.Fatalf("SetModTime calls = %#v", client.modTimes)
	}
	if dt := report.ModTime(ctx).Sub(modTime); dt <= -time.Second || dt >= time.Second {
		t.Fatalf("ModTime = %s, want %s within a second", report.ModTime(ctx), modTime)
	}
	// A second write at the same second is a no-op, as rmapi cannot report
	// anything finer.
	if err := report.SetModTime(ctx, modTime.Add(100*time.Millisecond)); err != nil || len(client.modTimes) != 1 {
		t.Fatalf("repeated SetModTime = %v, calls %d", err, len(client.modTimes))
	}
	listed := listedRemotes(t, sourceView, "Personal")["Personal/Report.pdf"]
	if !listed.ModTime(ctx).Equal(report.ModTime(ctx)) || listed.item.Version != 4 {
		t.Fatalf("listed after SetModTime = version %d time %s", listed.item.Version, listed.ModTime(ctx))
	}
	if got := client.downloadCount("report"); got != downloads {
		t.Fatalf("SetModTime re-downloaded the document: %d downloads, want %d", got, downloads)
	}
}

func TestSourceViewPutListsImportUnderItsSourceName(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{"personal": {ID: "personal", Name: "Personal", Kind: ItemDirectory}}}
	sourceView := newSourceViewFs(t, client)
	content := validPDF()
	modTime := time.Date(2026, time.March, 4, 5, 6, 7, 890_000_000, time.UTC)
	src := object.NewStaticObjectInfo("Personal/Quarterly Report.PDF", modTime, int64(len(content)), true, nil, nil)

	uploaded, err := sourceView.Put(ctx, bytes.NewReader(content), src)
	if err != nil {
		t.Fatal(err)
	}
	got := uploaded.(*Object)
	if got.Remote() != "Personal/Quarterly Report.PDF" || got.item.Name != "Quarterly Report.PDF" || got.Size() != int64(len(content)) {
		t.Fatalf("Put = %q (visible %q, size %d)", got.Remote(), got.item.Name, got.Size())
	}
	if sum, _ := got.Hash(ctx, hash.MD5); sum != md5Hex(content) {
		t.Fatalf("Put MD5 = %q", sum)
	}
	if len(client.modTimes) != 1 || client.modTimes[0].ID != got.item.ID || !client.modTimes[0].ModTime.Equal(modTime) {
		t.Fatalf("modification time writes = %#v", client.modTimes)
	}

	listed := listedRemotes(t, sourceView, "Personal")["Personal/Quarterly Report.PDF"]
	if listed == nil || listed.item.ID != got.item.ID || listed.Size() != int64(len(content)) {
		t.Fatalf("listing after Put = %v", listed)
	}
	if dt := listed.ModTime(ctx).Sub(modTime); dt <= -sourceView.Precision() || dt >= sourceView.Precision() {
		t.Fatalf("listed time %s is not within %s of %s", listed.ModTime(ctx), sourceView.Precision(), modTime)
	}

	// A repeat is adopted, not uploaded again.
	if _, err := sourceView.Put(ctx, bytes.NewReader(content), src); err != nil {
		t.Fatal(err)
	}
	if len(client.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(client.uploads))
	}
	assertUploadTempEmpty(t, sourceView.uploadTempDir)
}

func TestSourceViewPutRefusesNamesThatCollideInTheRMDOCView(t *testing.T) {
	ctx := context.Background()
	client := sourceViewClient(t)
	client.items["notebook"] = Item{ID: "notebook", Name: "Plan.pdf", ParentID: "personal", Kind: ItemDocument, Version: 1}
	client.contents["notebook"] = notebookArchive(t, "notebook")
	sourceView := newSourceViewFs(t, client)

	content := validPDF()
	src := object.NewStaticObjectInfo("Personal/Plan.pdf", time.Now(), int64(len(content)), true, nil, nil)
	_, err := sourceView.Put(ctx, bytes.NewReader(content), src)
	if err == nil || fserrors.IsRetryError(err) || !fserrors.IsNoRetryError(err) || !errors.Is(err, errDestinationExists) {
		t.Fatalf("Put next to a Plan.pdf notebook = %v, want a non-retryable collision", err)
	}

	rmdoc := validRMDOC(t, uploadDocumentID, 64)
	rmdocSrc := object.NewStaticObjectInfo("Personal/Plan.rmdoc", time.Now(), int64(len(rmdoc)), true, nil, nil)
	if _, err := sourceView.Put(ctx, bytes.NewReader(rmdoc), rmdocSrc); err != nil {
		t.Fatalf("Put of an unrelated .rmdoc = %v", err)
	}
	if len(client.uploads) != 1 {
		t.Fatalf("uploads = %#v", client.uploads)
	}
}

func TestSourceViewPutRefusesRMDOCThatWouldBeListedByItsPayload(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{}}
	sourceView := newSourceViewFs(t, client)
	content := payloadArchive(t, uploadDocumentID, "pdf", validPDF())
	src := object.NewStaticObjectInfo("Report.pdf.rmdoc", time.Now(), int64(len(content)), true, nil, nil)
	if _, err := sourceView.Put(ctx, bytes.NewReader(content), src); !fserrors.IsNoRetryError(err) || !errors.Is(err, errInvalidImport) {
		t.Fatalf("Put = %v, want a non-retryable refusal", err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("uploads = %#v", client.uploads)
	}
}

func TestSourceViewMoveNaming(t *testing.T) {
	tests := []struct {
		name        string
		destination string
		wantName    string
		wantErr     string
	}{
		{name: "rename keeps the payload view", destination: "Personal/Annual.pdf", wantName: "Annual.pdf"},
		{name: "rename into the rmdoc view", destination: "Personal/Annual.rmdoc", wantName: "Annual"},
		{name: "rmdoc name that would list as the payload", destination: "Personal/Annual.pdf.rmdoc", wantErr: "would be listed as"},
		{name: "other extension", destination: "Personal/Annual.epub", wantErr: "must end in .pdf or .rmdoc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := sourceViewClient(t)
			sourceView := newSourceViewFs(t, client)
			src, err := sourceView.NewObject(ctx, "Personal/Report.pdf")
			if err != nil {
				t.Fatal(err)
			}
			moved, err := sourceView.Move(ctx, src, test.destination)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Move error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if client.items["report"].Name != test.wantName {
				t.Fatalf("visible name = %q, want %q", client.items["report"].Name, test.wantName)
			}
			listed := listedRemotes(t, sourceView, "Personal")[test.destination]
			if listed == nil || listed.item.ID != "report" || listed.Size() != moved.Size() {
				t.Fatalf("listing after move: %v, moved size %d", listed, moved.Size())
			}
		})
	}
}

// replaceFailingClient fails every payload replacement in transport.
type replaceFailingClient struct {
	*fakeClient
}

func (c replaceFailingClient) Replace(context.Context, string, string, time.Time) (Item, error) {
	return Item{}, errors.New("connection reset")
}

func TestSourceViewUpdate(t *testing.T) {
	ctx := context.Background()
	changedPDF := []byte("%PDF-1.7\n% revised\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
	modTime := time.Date(2026, time.November, 24, 18, 0, 0, 0, time.UTC)
	open := func(t *testing.T, backend *Fs, remote string) *Object {
		t.Helper()
		found, err := backend.NewObject(ctx, remote)
		if err != nil {
			t.Fatal(err)
		}
		return found.(*Object)
	}
	update := func(o *Object, remote string, content []byte) error {
		src := object.NewStaticObjectInfo(remote, modTime, int64(len(content)), true, nil, nil)
		return o.Update(ctx, bytes.NewReader(content), src)
	}

	t.Run("replaces the payload in place", func(t *testing.T) {
		client := sourceViewClient(t)
		sourceView := newSourceViewFs(t, client)
		report := open(t, sourceView, "Personal/Report.pdf")
		if err := update(report, "Personal/Report.pdf", changedPDF); err != nil {
			t.Fatal(err)
		}
		if len(client.replaces) != 1 || client.replaces[0].ID != "report" || !bytes.Equal(client.replaces[0].Payload, changedPDF) || !client.replaces[0].ModTime.Equal(modTime) {
			t.Fatalf("replace calls = %#v", client.replaces)
		}
		if report.item.ID != "report" || report.item.Version != 4 || report.Size() != int64(len(changedPDF)) || !report.ModTime(ctx).Equal(modTime) {
			t.Fatalf("updated object = %#v", report)
		}
		// The new version is served from the promoted cache, not downloaded.
		reader, err := report.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(reader)
		_ = reader.Close()
		if !bytes.Equal(got, changedPDF) || client.downloadCount("report") != 1 {
			t.Fatalf("read after update = %q with %d downloads", got, client.downloadCount("report"))
		}
		listed := listedRemotes(t, sourceView, "Personal")["Personal/Report.pdf"]
		if sum, _ := listed.Hash(ctx, hash.MD5); sum != md5Hex(changedPDF) || listed.Size() != int64(len(changedPDF)) {
			t.Fatalf("listed after update: size %d MD5 %q", listed.Size(), sum)
		}
	})
	t.Run("unchanged payload only updates the modification time", func(t *testing.T) {
		client := sourceViewClient(t)
		sourceView := newSourceViewFs(t, client)
		report := open(t, sourceView, "Personal/Report.pdf")
		if err := update(report, "Personal/Report.pdf", validPDF()); err != nil {
			t.Fatal(err)
		}
		if len(client.replaces) != 0 || len(client.modTimes) != 1 || !report.ModTime(ctx).Equal(modTime) {
			t.Fatalf("replaces %#v, time writes %#v", client.replaces, client.modTimes)
		}
	})
	t.Run("transport failure is retryable", func(t *testing.T) {
		client := sourceViewClient(t)
		backend, err := newFsView(ctx, "test", "", replaceFailingClient{client}, t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		err = update(open(t, backend, "Personal/Report.pdf"), "Personal/Report.pdf", changedPDF)
		if err == nil || fserrors.IsNoRetryError(err) {
			t.Fatalf("Update = %v, want a retryable error", err)
		}
		assertUploadTempEmpty(t, backend.uploadTempDir)
	})

	rejections := []struct {
		name       string
		sourceView bool
		remote     string
		srcRemote  string
		content    []byte
		want       error
	}{
		{name: "rmdoc view", remote: "Personal/Report.pdf.rmdoc", srcRemote: "Personal/Report.pdf.rmdoc", content: changedPDF, want: fs.ErrorNotImplemented},
		{name: "notebook in source view", sourceView: true, remote: "Personal/Notes.rmdoc", srcRemote: "Personal/Notes.rmdoc", content: changedPDF, want: fs.ErrorNotImplemented},
		{name: "PDF without its extension", sourceView: true, remote: "Personal/Scan.rmdoc", srcRemote: "Personal/Scan.rmdoc", content: changedPDF, want: fs.ErrorNotImplemented},
		{name: "invalid PDF", sourceView: true, remote: "Personal/Report.pdf", srcRemote: "Personal/Report.pdf", content: []byte("not a PDF"), want: errInvalidImport},
		{name: "EPUB content for a PDF", sourceView: true, remote: "Personal/Report.pdf", srcRemote: "Personal/Report.pdf", content: validEPUB(t), want: errInvalidImport},
		{name: "PDF content for an EPUB", sourceView: true, remote: "Personal/Book.epub", srcRemote: "Personal/Book.epub", content: changedPDF, want: errInvalidImport},
		{name: "format change by name", sourceView: true, remote: "Personal/Report.pdf", srcRemote: "Personal/Report.epub", content: validEPUB(t), want: errInvalidImport},
	}
	for _, test := range rejections {
		t.Run("rejects "+test.name, func(t *testing.T) {
			client := sourceViewClient(t)
			backend, err := newFsView(ctx, "test", "", client, t.TempDir(), test.sourceView)
			if err != nil {
				t.Fatal(err)
			}
			err = update(open(t, backend, test.remote), test.srcRemote, test.content)
			if !errors.Is(err, test.want) || !fserrors.IsNoRetryError(err) {
				t.Fatalf("Update = %v, want non-retryable %v", err, test.want)
			}
			if len(client.replaces) != 0 || len(client.modTimes) != 0 {
				t.Fatalf("rejected update reached the client: %#v %#v", client.replaces, client.modTimes)
			}
			assertUploadTempEmpty(t, backend.uploadTempDir)
		})
	}
	t.Run("rejects empty content as unuploadable", func(t *testing.T) {
		client := sourceViewClient(t)
		sourceView := newSourceViewFs(t, client)
		if err := update(open(t, sourceView, "Personal/Report.pdf"), "Personal/Report.pdf", nil); !errors.Is(err, fs.ErrorCantUploadEmptyFiles) {
			t.Fatalf("Update = %v", err)
		}
	})
}

// TestSourceViewSyncMirrorsDirectory runs rclone sync from a local directory
// through the scenario the option exists for: import, an idle re-run, a
// changed source, and a removed source.
func TestSourceViewSyncMirrorsDirectory(t *testing.T) {
	ctx := context.Background()
	fi, err := filter.NewFilter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fi.AddRule("+ *.{pdf,epub}"); err != nil {
		t.Fatal(err)
	}
	if err := fi.AddRule("- *"); err != nil {
		t.Fatal(err)
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
	firstTime := time.Date(2026, time.November, 1, 9, 15, 30, 456_000_000, time.UTC)
	write("Report.pdf", validPDF(), firstTime)
	write("Book.epub", validEPUB(t), firstTime)
	write("todo.txt", []byte("not importable"), firstTime)
	src, err := fs.NewFs(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{
		items: map[string]Item{
			"personal": {ID: "personal", Name: "Personal", Kind: ItemDirectory},
			"notebook": {ID: "notebook", Name: "Journal", ParentID: "personal", Kind: ItemDocument, Version: 1},
		},
		contents: map[string][]byte{"notebook": notebookArchive(t, "notebook")},
	}
	dst, err := newFsView(ctx, "test", "Personal", client, t.TempDir(), true)
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
	documentIDs := func() map[string]string {
		t.Helper()
		ids := make(map[string]string)
		for remote, object := range listedRemotes(t, dst, "") {
			ids[remote] = object.item.ID
		}
		return ids
	}

	stats := run("import")
	if stats.GetTransfers() != 2 || len(client.uploads) != 2 {
		t.Fatalf("import transferred %d with %d uploads", stats.GetTransfers(), len(client.uploads))
	}
	imported := documentIDs()
	if len(imported) != 3 || imported["Report.pdf"] == "" || imported["Book.epub"] == "" || imported["Journal.rmdoc"] != "notebook" {
		t.Fatalf("after import: %v", imported)
	}
	if err := operations.Check(ctx, &operations.CheckOpt{Fdst: dst, Fsrc: src, OneWay: true}); err != nil {
		t.Fatalf("check after import: %v", err)
	}

	stats = run("idle")
	if stats.GetTransfers() != 0 || stats.GetDeletes() != 0 || len(client.uploads) != 2 || len(client.replaces) != 0 || len(client.removes) != 0 {
		t.Fatalf("idle sync: %d transfers, %d deletes, uploads %d, replaces %d, removes %d", stats.GetTransfers(), stats.GetDeletes(), len(client.uploads), len(client.replaces), len(client.removes))
	}

	changed := []byte("%PDF-1.7\n% second edition\n%%EOF\n")
	write("Report.pdf", changed, firstTime.Add(time.Hour))
	stats = run("change")
	if stats.GetTransfers() != 1 || len(client.replaces) != 1 || len(client.uploads) != 2 {
		t.Fatalf("change sync: %d transfers, replaces %#v, uploads %d", stats.GetTransfers(), client.replaces, len(client.uploads))
	}
	afterChange := documentIDs()
	if afterChange["Report.pdf"] != imported["Report.pdf"] {
		t.Fatalf("Report.pdf UUID changed from %q to %q", imported["Report.pdf"], afterChange["Report.pdf"])
	}
	report, err := dst.NewObject(ctx, "Report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := report.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(reader)
	_ = reader.Close()
	if !bytes.Equal(content, changed) {
		t.Fatalf("Report.pdf after change = %q", content)
	}
	if stats = run("idle after change"); stats.GetTransfers() != 0 || len(client.replaces) != 1 || len(client.modTimes) != 2 {
		t.Fatalf("idle sync after change: %d transfers, replaces %d, time writes %d", stats.GetTransfers(), len(client.replaces), len(client.modTimes))
	}

	if err := os.Remove(filepath.Join(sourceDir, "Book.epub")); err != nil {
		t.Fatal(err)
	}
	stats = run("delete")
	if stats.GetDeletes() != 1 || len(client.removes) != 1 || client.removes[0] != imported["Book.epub"] {
		t.Fatalf("delete sync: %d deletes, removes %#v, want %q", stats.GetDeletes(), client.removes, imported["Book.epub"])
	}
	if remaining := documentIDs(); len(remaining) != 2 || remaining["Journal.rmdoc"] != "notebook" {
		t.Fatalf("after delete: %v", remaining)
	}
}

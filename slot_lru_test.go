package reasoningeffort

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/hirochachacha/go-smb2"
	"go.uber.org/zap"
)

// fakeFS is an in-memory SlotFS for unit tests.
type fakeFS struct {
	mu       sync.Mutex
	files    map[string][]byte
	modtimes map[string]time.Time
	seedSeq  time.Time
	writes   int
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:    make(map[string][]byte),
		modtimes: make(map[string]time.Time),
		seedSeq:  time.Unix(0, 0).UTC(),
	}
}

// seed adds a file with a modification time that increases per call, so
// files seeded in order are ordered by modification time in the same order.
func (f *fakeFS) seed(name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedSeq = f.seedSeq.Add(time.Second)
	f.files[name] = data
	f.modtimes[name] = f.seedSeq
}

// seedAt adds a file with an explicit modification time.
func (f *fakeFS) seedAt(name string, data []byte, modtime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = data
	f.modtimes[name] = modtime
}

// writeCount returns the number of WriteFile calls so far.
func (f *fakeFS) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *fakeFS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[name]; !ok {
		return &os.PathError{Op: "remove", Path: name, Err: syscall.ENOENT}
	}
	delete(f.files, name)
	delete(f.modtimes, name)
	return nil
}

func (f *fakeFS) Stat(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[name]
	return ok, nil
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: syscall.ENOENT}
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeFS) WriteFile(name string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.files[name] = append([]byte(nil), data...)
	return nil
}

func (f *fakeFS) Rename(oldname, newname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[oldname]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldname, Err: syscall.ENOENT}
	}
	delete(f.files, oldname)
	f.files[newname] = data
	return nil
}

func (f *fakeFS) ListFiles() ([]SlotFileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files := make([]SlotFileInfo, 0, len(f.files))
	for name, mt := range f.modtimes {
		files = append(files, SlotFileInfo{Name: name, ModTime: mt})
	}
	return files, nil
}

func saveBody(filename string) []byte {
	return []byte(`{"filename":` + `"` + filename + `"}`)
}

// lruOrder returns the current LRU order, most recently used first. It
// completes the deferred background load first (if it has not run yet),
// so it can be called immediately after newSlotLRU.
func lruOrder(l *slotLRU) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureLoaded()
	out := make([]string, 0, l.list.Len())
	for el := l.list.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(string))
	}
	return out
}

func TestSlotLRURecordOrderAndEviction(t *testing.T) {
	fs := newFakeFS()
	l := newSlotLRU(fs, 2, zap.NewNop())

	// llama-server creates the file on save, i.e. after the LRU is
	// initialized; seed each file right after its record to mirror that.
	l.record(saveBody("a.bin"))
	fs.seed("a.bin", []byte("x"))
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"a.bin"}) {
		t.Fatalf("after save a: got %v", got)
	}
	l.record(saveBody("b.bin"))
	fs.seed("b.bin", []byte("x"))
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"b.bin", "a.bin"}) {
		t.Fatalf("after save b: got %v", got)
	}
	l.record(saveBody("c.bin"))
	fs.seed("c.bin", []byte("x"))
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"c.bin", "b.bin"}) {
		t.Fatalf("after save c: got %v, want [c.bin b.bin]", got)
	}
	if ok, _ := fs.Stat("a.bin"); ok {
		t.Errorf("evicted file a.bin still exists")
	}
	if ok, _ := fs.Stat("b.bin"); !ok {
		t.Errorf("b.bin should still exist")
	}
}

func TestSlotLRURestoreMovesToMRU(t *testing.T) {
	fs := newFakeFS()
	fs.seed("a.bin", []byte("x"))
	fs.seed("b.bin", []byte("x"))
	l := newSlotLRU(fs, 5, zap.NewNop())

	l.record(saveBody("a.bin"))
	l.record(saveBody("b.bin"))
	// a is now LRU; restoring it must move it to MRU.
	l.record(saveBody("a.bin"))
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"a.bin", "b.bin"}) {
		t.Fatalf("after restore a: got %v, want [a.bin b.bin]", got)
	}
}

func TestSlotLRURestoreUnknownFileAdds(t *testing.T) {
	fs := newFakeFS()
	fs.seed("z.bin", []byte("x"))
	l := newSlotLRU(fs, 5, zap.NewNop())

	l.record(saveBody("z.bin"))
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"z.bin"}) {
		t.Fatalf("after restore z: got %v", got)
	}
}

func TestSlotLRUNoEvictionWhenMaxZero(t *testing.T) {
	fs := newFakeFS()
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		fs.seed(n, []byte("x"))
	}
	l := newSlotLRU(fs, 0, zap.NewNop())

	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		l.record(saveBody(n))
	}
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"c.bin", "b.bin", "a.bin"}) {
		t.Fatalf("with max=0: got %v, want all three tracked", got)
	}
	if ok, _ := fs.Stat("a.bin"); !ok {
		t.Errorf("a.bin should not be deleted with max=0")
	}
}

func TestSlotLRUPersistRoundTrip(t *testing.T) {
	fs := newFakeFS()
	fs.seed("a.bin", []byte("x"))
	fs.seed("b.bin", []byte("x"))
	l := newSlotLRU(fs, 5, zap.NewNop())
	l.record(saveBody("a.bin"))
	l.record(saveBody("b.bin"))

	l2 := newSlotLRU(fs, 5, zap.NewNop())
	if got := lruOrder(l2); !reflect.DeepEqual(got, []string{"b.bin", "a.bin"}) {
		t.Fatalf("reloaded order: got %v, want [b.bin a.bin]", got)
	}
}

func TestSlotLRUPersistNoTmpLeftBehind(t *testing.T) {
	fs := newFakeFS()
	l := newSlotLRU(fs, 5, zap.NewNop())
	l.record(saveBody("a.bin"))

	files, err := fs.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name, slotLRUStateFileTmpPrefix) {
			t.Fatalf("temporary state file %q left behind", f.Name)
		}
	}
}

func TestSlotLRUConcurrentPersistsDoNotCollide(t *testing.T) {
	// Two instances sharing one directory (e.g. overlapping plugin
	// instances during a Caddy reload) must not clobber each other's
	// temporary files; the last rename wins.
	fs := newFakeFS()
	l1 := newSlotLRU(fs, 5, zap.NewNop())
	l2 := newSlotLRU(fs, 5, zap.NewNop())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			l1.record(saveBody("a.bin"))
		}()
		go func() {
			defer wg.Done()
			l2.record(saveBody("b.bin"))
		}()
	}
	wg.Wait()

	files, err := fs.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name, slotLRUStateFileTmpPrefix) {
			t.Fatalf("temporary state file %q left behind", f.Name)
		}
	}
	data, err := fs.ReadFile(slotLRUStateFile)
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	var st slotLRUState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state file corrupt: %v (%s)", err, data)
	}
	if len(st.Entries) == 0 {
		t.Fatal("state file is empty")
	}
}

func TestSlotLRULoadDropsMissingFiles(t *testing.T) {
	fs := newFakeFS()
	fs.seed("a.bin", []byte("x"))
	// b.bin is in the state file but not on disk.
	state, _ := json.Marshal(slotLRUState{Entries: []string{"b.bin", "a.bin"}})
	fs.seed(slotLRUStateFile, state)

	l := newSlotLRU(fs, 5, zap.NewNop())
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"a.bin"}) {
		t.Fatalf("after load: got %v, want [a.bin]", got)
	}
}

func TestSlotLRULoadEvictsOverLimit(t *testing.T) {
	fs := newFakeFS()
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		fs.seed(n, []byte("x"))
	}
	state, _ := json.Marshal(slotLRUState{Entries: []string{"c.bin", "b.bin", "a.bin"}})
	fs.seed(slotLRUStateFile, state)

	l := newSlotLRU(fs, 2, zap.NewNop())
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"c.bin", "b.bin"}) {
		t.Fatalf("after load with max=2: got %v, want [c.bin b.bin]", got)
	}
	if ok, _ := fs.Stat("a.bin"); ok {
		t.Errorf("a.bin should have been evicted on load")
	}
}

func TestSlotLRULoadUnchangedDoesNotRewrite(t *testing.T) {
	fs := newFakeFS()
	fs.seed("a.bin", []byte("x"))
	state, _ := json.Marshal(slotLRUState{Entries: []string{"a.bin"}})
	fs.seed(slotLRUStateFile, state)

	l := newSlotLRU(fs, 5, zap.NewNop())
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"a.bin"}) {
		t.Fatalf("got %v, want [a.bin]", got)
	}
	if n := fs.writeCount(); n != 0 {
		t.Fatalf("state file rewritten although nothing changed (writes=%d)", n)
	}
}

func TestSlotLRULoadCleansStaleState(t *testing.T) {
	fs := newFakeFS()
	// The state file lists a.bin, which is not on disk.
	state, _ := json.Marshal(slotLRUState{Entries: []string{"a.bin"}})
	fs.seed(slotLRUStateFile, state)

	l := newSlotLRU(fs, 5, zap.NewNop())
	if got := lruOrder(l); len(got) != 0 {
		t.Fatalf("expected empty LRU, got %v", got)
	}
	// The stale state file must be rewritten empty.
	data, err := fs.ReadFile(slotLRUStateFile)
	if err != nil {
		t.Fatal(err)
	}
	var st slotLRUState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state file not valid json: %v", err)
	}
	if len(st.Entries) != 0 {
		t.Fatalf("stale state file not cleaned: %v", st.Entries)
	}
}

func TestSlotLRULoadCorruptStateIsReplaced(t *testing.T) {
	fs := newFakeFS()
	fs.seed(slotLRUStateFile, []byte("not json"))

	l := newSlotLRU(fs, 5, zap.NewNop())
	if got := lruOrder(l); len(got) != 0 {
		t.Fatalf("expected empty LRU, got %v", got)
	}
	// The corrupt state file must be replaced with valid state.
	data, err := fs.ReadFile(slotLRUStateFile)
	if err != nil {
		t.Fatal(err)
	}
	var st slotLRUState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("corrupt state file was not replaced: %v", err)
	}
}

func TestSlotLRUReloadAfterEviction(t *testing.T) {
	fs := newFakeFS()
	l := newSlotLRU(fs, 1, zap.NewNop())
	// llama-server creates the file on save, i.e. after the LRU is
	// initialized; seed each file right after its record to mirror that.
	l.record(saveBody("a.bin"))
	fs.seed("a.bin", []byte("x"))
	l.record(saveBody("b.bin"))
	fs.seed("b.bin", []byte("x"))
	// Only b.bin remains on disk; a.bin was evicted.
	l2 := newSlotLRU(fs, 1, zap.NewNop())
	if got := lruOrder(l2); !reflect.DeepEqual(got, []string{"b.bin"}) {
		t.Fatalf("after reload: got %v, want [b.bin]", got)
	}
}

func TestSlotLRUAdoptsUntrackedByMtime(t *testing.T) {
	fs := newFakeFS()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs.seedAt("old.bin", []byte("x"), base)
	fs.seedAt("mid.bin", []byte("x"), base.Add(time.Hour))
	fs.seedAt("new.bin", []byte("x"), base.Add(2*time.Hour))

	l := newSlotLRU(fs, 5, zap.NewNop())
	// Adopted files are ordered by modification time, newest first.
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"new.bin", "mid.bin", "old.bin"}) {
		t.Fatalf("adopted order: got %v, want [new.bin mid.bin old.bin]", got)
	}
}

func TestSlotLRUAdoptionEvictsOldest(t *testing.T) {
	fs := newFakeFS()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fs.seedAt("old.bin", []byte("x"), base)
	fs.seedAt("mid.bin", []byte("x"), base.Add(time.Hour))
	fs.seedAt("new.bin", []byte("x"), base.Add(2*time.Hour))

	l := newSlotLRU(fs, 2, zap.NewNop())
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"new.bin", "mid.bin"}) {
		t.Fatalf("after adoption with max=2: got %v, want [new.bin mid.bin]", got)
	}
	if ok, _ := fs.Stat("old.bin"); ok {
		t.Errorf("oldest untracked file old.bin should have been evicted")
	}
}

func TestSlotLRUAdoptionKeepsTrackedFirst(t *testing.T) {
	fs := newFakeFS()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// z.bin has the newest mtime but is untracked; it must still be placed
	// after the tracked entries. The state file itself must not be adopted.
	fs.seedAt("x.bin", []byte("x"), base)
	fs.seedAt("y.bin", []byte("x"), base.Add(time.Hour))
	fs.seedAt("z.bin", []byte("x"), base.Add(2*time.Hour))
	state, _ := json.Marshal(slotLRUState{Entries: []string{"x.bin", "y.bin"}})
	fs.seed(slotLRUStateFile, state)

	l := newSlotLRU(fs, 5, zap.NewNop())
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"x.bin", "y.bin", "z.bin"}) {
		t.Fatalf("got %v, want [x.bin y.bin z.bin]", got)
	}
}

func TestSlotLRUAdoptLocalFSByMtime(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, modtime := range map[string]time.Time{
		"old.bin": base,
		"new.bin": base.Add(time.Hour),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(dir, name), modtime, modtime); err != nil {
			t.Fatal(err)
		}
	}

	l := newSlotLRU(newLocalFS(dir), 5, zap.NewNop())
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"new.bin", "old.bin"}) {
		t.Fatalf("adopted order: got %v, want [new.bin old.bin]", got)
	}
	// Adoption must be persisted so the files stay tracked across restarts.
	if got := lruOrder(newSlotLRU(newLocalFS(dir), 5, zap.NewNop())); !reflect.DeepEqual(got, []string{"new.bin", "old.bin"}) {
		t.Fatalf("reloaded order: got %v, want [new.bin old.bin]", got)
	}
}

func TestSlotLRURejectsBadFilenames(t *testing.T) {
	fs := newFakeFS()
	fs.seed("a.bin", []byte("x"))
	l := newSlotLRU(fs, 5, zap.NewNop())
	l.record(saveBody("a.bin"))

	for _, bad := range []string{"", ".", "..", "../x", "a/b", `a\b`, slotLRUStateFile, slotLRUStateFileTmpPrefix + "deadbeef"} {
		body, _ := json.Marshal(slotRequestBody{Filename: bad})
		l.record(body)
	}
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"a.bin"}) {
		t.Fatalf("bad filenames changed the LRU: got %v", got)
	}
}

func TestSlotLRUDelete(t *testing.T) {
	fs := newFakeFS()
	fs.seed("a.bin", []byte("x"))
	fs.seed("b.bin", []byte("x"))
	l := newSlotLRU(fs, 5, zap.NewNop())
	l.record(saveBody("a.bin"))
	l.record(saveBody("b.bin"))

	deleted, err := l.delete("a.bin")
	if err != nil || !deleted {
		t.Fatalf("delete a.bin: deleted=%v err=%v", deleted, err)
	}
	if ok, _ := fs.Stat("a.bin"); ok {
		t.Errorf("a.bin still exists after delete")
	}
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"b.bin"}) {
		t.Fatalf("after delete: got %v, want [b.bin]", got)
	}

	// Idempotent: deleting a missing file is not an error.
	deleted, err = l.delete("a.bin")
	if err != nil || deleted {
		t.Fatalf("second delete a.bin: deleted=%v err=%v", deleted, err)
	}

	// A file on disk but not in the table is still deleted.
	deleted, err = l.delete("b.bin")
	if err != nil || !deleted {
		t.Fatalf("delete b.bin: deleted=%v err=%v", deleted, err)
	}
	if got := lruOrder(l); len(got) != 0 {
		t.Fatalf("table should be empty, got %v", got)
	}

	for _, bad := range []string{"../x", slotLRUStateFile, slotLRUStateFileTmpPrefix + "deadbeef"} {
		if _, err := l.delete(bad); err == nil {
			t.Fatalf("expected error for invalid filename %q, got nil", bad)
		}
	}
}

// runSlotRequest builds a POST request to path with the given body, runs
// the middleware, and returns both the response recorder and the
// downstream capture.
func runSlotRequest(t *testing.T, m ReasoningEffort, body string, path string) (*httptest.ResponseRecorder, *captureHandler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	cap := &captureHandler{}
	next := caddyhttp.HandlerFunc(cap.ServeHTTP)
	if err := m.ServeHTTP(rec, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return rec, cap
}

// newSlotMiddleware builds a ReasoningEffort with the slot LRU enabled on
// the given directory and limit.
func newSlotMiddleware(t *testing.T, dir string, max int) ReasoningEffort {
	t.Helper()
	m := ReasoningEffort{Path: defaultPath, SlotSavePath: dir, SlotLRUMax: max}
	m.slotLRU = newSlotLRU(newLocalFS(dir), max, zap.NewNop())
	return m
}

// waitForSlotLRUState polls the persisted state file until it holds the
// expected order (most recently used first).
func waitForSlotLRUState(t *testing.T, dir string, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(dir, slotLRUStateFile))
		if err == nil {
			last = data
			var st slotLRUState
			if json.Unmarshal(data, &st) == nil && reflect.DeepEqual(st.Entries, want) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for slot LRU state %v, last state: %s", want, last)
}

func TestServeSlotsSaveTracksLRU(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 5)

	rec, cap := runSlotRequest(t, m, `{"filename":"a.bin"}`, "/slots/0?action=save")

	// The request must be forwarded downstream with the body untouched.
	if cap.path != "/slots/0" {
		t.Errorf("downstream path: got %q, want %q", cap.path, "/slots/0")
	}
	if string(cap.body) != `{"filename":"a.bin"}` {
		t.Errorf("downstream body altered: %q", cap.body)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response code: got %d", rec.Code)
	}
	waitForSlotLRUState(t, dir, []string{"a.bin"})
}

func TestServeSlotsEvictionChain(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 2)

	// Wait for each async update before the next save so the goroutines
	// apply in request order.
	want := []string{}
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		if _, cap := runSlotRequest(t, m, `{"filename":"`+n+`"}`, "/slots/0?action=save"); cap.path == "" {
			t.Fatalf("save %s was not forwarded", n)
		}
		// In production llama-server creates the file on save.
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		want = append([]string{n}, want...)
		if len(want) > 2 {
			want = want[:2]
		}
		waitForSlotLRUState(t, dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.bin")); !os.IsNotExist(err) {
		t.Errorf("a.bin should have been evicted from disk")
	}
}

func TestServeSlotsSavePathOnlyNoEviction(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 0)

	want := []string{}
	for _, n := range []string{"a.bin", "b.bin"} {
		runSlotRequest(t, m, `{"filename":"`+n+`"}`, "/slots/0?action=save")
		// In production llama-server creates the file on save.
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		want = append([]string{n}, want...)
		waitForSlotLRUState(t, dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.bin")); err != nil {
		t.Errorf("a.bin should not be evicted with max=0")
	}
}

func TestServeSlotsDeleteShortCircuits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newSlotMiddleware(t, dir, 5)
	m.slotLRU.record(saveBody("a.bin"))

	rec, cap := runSlotRequest(t, m, `{"filename":"a.bin"}`, "/slots/0?action=delete")

	// delete must not reach the upstream.
	if cap.path != "" || cap.body != nil {
		t.Fatalf("delete was forwarded downstream: path=%q body=%q", cap.path, cap.body)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("response code: got %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v (%s)", err, rec.Body.String())
	}
	if resp["filename"] != "a.bin" || resp["deleted"] != true {
		t.Errorf("unexpected response: %v", resp)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.bin")); !os.IsNotExist(err) {
		t.Errorf("a.bin still exists after delete")
	}
	waitForSlotLRUState(t, dir, []string{})
}

func TestServeSlotsDeleteMissingFile(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 5)

	rec, _ := runSlotRequest(t, m, `{"filename":"nope.bin"}`, "/slots/1?action=delete")
	if rec.Code != http.StatusOK {
		t.Fatalf("response code: got %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if resp["deleted"] != false {
		t.Errorf("expected deleted=false, got %v", resp)
	}
}

func TestServeSlotsDeleteInvalidFilename(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 5)

	rec, _ := runSlotRequest(t, m, `{"filename":"../x"}`, "/slots/0?action=delete")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("response code: got %d, want 400", rec.Code)
	}
}

func TestServeSlotsDeleteReservedName(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 5)

	rec, _ := runSlotRequest(t, m, `{"filename":"`+slotLRUStateFile+`"}`, "/slots/0?action=delete")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("response code: got %d, want 400", rec.Code)
	}
}

func TestServeSlotsDeleteOversizedBody(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 5)

	body := `{"filename":"` + strings.Repeat("a", 600) + `"}`
	rec, _ := runSlotRequest(t, m, body, "/slots/0?action=delete")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("response code: got %d, want 400", rec.Code)
	}
}

func TestServeSlotsSaveReservedNameNotTracked(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 5)
	m.slotLRU.record(saveBody("a.bin"))

	// The request is still forwarded (no pre-check); the LRU just must not
	// track the reserved name.
	rec, cap := runSlotRequest(t, m, `{"filename":"`+slotLRUStateFile+`"}`, "/slots/0?action=save")
	if cap.path != "/slots/0" {
		t.Fatalf("request was not forwarded downstream")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response code: got %d", rec.Code)
	}
	waitForSlotLRUState(t, dir, []string{"a.bin"})
}

func TestServeSlotsErasePassesThrough(t *testing.T) {
	dir := t.TempDir()
	m := newSlotMiddleware(t, dir, 5)
	m.slotLRU.record(saveBody("a.bin"))

	rec, cap := runSlotRequest(t, m, `{}`, "/slots/0?action=erase")
	if cap.path != "/slots/0" {
		t.Fatalf("erase was not forwarded downstream")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response code: got %d", rec.Code)
	}
	// The LRU must be untouched: erase only clears the in-memory cache.
	waitForSlotLRUState(t, dir, []string{"a.bin"})
}

func TestServeSlotsDisabledPassesThrough(t *testing.T) {
	dir := t.TempDir()
	m := ReasoningEffort{Path: defaultPath, SlotSavePath: dir}
	// slotLRU is nil: the feature is disabled.

	_, cap := runSlotRequest(t, m, `{"filename":"a.bin"}`, "/slots/0?action=save")
	if cap.path != "/slots/0" {
		t.Fatalf("request was not forwarded downstream")
	}
	if _, err := os.Stat(filepath.Join(dir, slotLRUStateFile)); !os.IsNotExist(err) {
		t.Errorf("state file must not be written when the feature is disabled")
	}
}

func TestUnmarshalCaddyfileSlotOptions(t *testing.T) {
	input := `reasoning_effort {
		slot_save_path /tmp/slots
		slot_lru_max 8
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if m.SlotSavePath != "/tmp/slots" {
		t.Errorf("slot_save_path: got %q", m.SlotSavePath)
	}
	if m.SlotLRUMax != 8 {
		t.Errorf("slot_lru_max: got %d", m.SlotLRUMax)
	}
}

func TestUnmarshalCaddyfileSlotLRUMaxInvalid(t *testing.T) {
	input := `reasoning_effort {
		slot_lru_max abc
	}`
	d := caddyfile.NewTestDispenser(input)
	var m ReasoningEffort
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected error for non-integer slot_lru_max, got nil")
	}
}

func TestProvisionSlotLRUMaxWithoutPath(t *testing.T) {
	m := ReasoningEffort{SlotLRUMax: 3}
	if err := m.provisionSlotLRU(zap.NewNop()); err == nil {
		t.Fatal("expected error for slot_lru_max without slot_save_path, got nil")
	}
}

func TestProvisionSlotLRUNegativeMax(t *testing.T) {
	m := ReasoningEffort{SlotSavePath: t.TempDir(), SlotLRUMax: -1}
	if err := m.provisionSlotLRU(zap.NewNop()); err == nil {
		t.Fatal("expected error for negative slot_lru_max, got nil")
	}
}

func TestProvisionSlotLRUPathOnly(t *testing.T) {
	dir := t.TempDir()
	m := ReasoningEffort{SlotSavePath: dir}
	if err := m.provisionSlotLRU(zap.NewNop()); err != nil {
		t.Fatalf("provisionSlotLRU error: %v", err)
	}
	if m.slotLRU == nil {
		t.Fatal("slotLRU not initialized")
	}
}

func TestParseSMBURL(t *testing.T) {
	tests := []struct {
		raw     string
		want    smbURL
		wantErr bool
	}{
		{
			raw:  "smb://user:password@192.168.1.200/share",
			want: smbURL{user: "user", password: "password", server: "192.168.1.200:445", share: "share"},
		},
		{
			raw:  "smb://user@192.168.1.200/share/sub/dir",
			want: smbURL{user: "user", server: "192.168.1.200:445", share: "share", root: "sub/dir"},
		},
		{
			raw:  "smb://user:pass@host/share/",
			want: smbURL{user: "user", password: "pass", server: "host:445", share: "share"},
		},
		{
			raw:  "smb://domain;user:pass@host:1445/share",
			want: smbURL{domain: "domain", user: "user", password: "pass", server: "host:1445", share: "share"},
		},
		{
			raw:  "smb://user:p%40ss@host/share",
			want: smbURL{user: "user", password: "p@ss", server: "host:445", share: "share"},
		},
		{
			raw:  "smb://host/share",
			want: smbURL{server: "host:445", share: "share"},
		},
		{
			raw:     "smb://user@host",
			wantErr: true,
		},
		{
			raw:     "smb://user@host/",
			wantErr: true,
		},
		{
			raw:     "smb://host/",
			wantErr: true,
		},
		{
			raw:     "http://host/share",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		got, err := parseSMBURL(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseSMBURL(%q): expected error, got %+v", tt.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSMBURL(%q): %v", tt.raw, err)
			continue
		}
		if *got != tt.want {
			t.Errorf("parseSMBURL(%q) = %+v, want %+v", tt.raw, *got, tt.want)
		}
	}
}

func TestNewSMBFSFromURLRequiresUser(t *testing.T) {
	if _, err := newSMBFSFromURL("smb://192.168.1.200/share", zap.NewNop()); err == nil {
		t.Fatal("expected error for smb URL without username, got nil")
	}
}

func TestNewSMBFSFromURLDialFailure(t *testing.T) {
	// Port 1 on localhost refuses connections immediately. The failure is
	// tolerated: the FS is returned disconnected and reconnects on first
	// use.
	fs, err := newSMBFSFromURL("smb://user:pass@127.0.0.1:1/share", zap.NewNop())
	if err != nil {
		t.Fatalf("dial failure should be tolerated, got error: %v", err)
	}
	if fs.conn.Load() != nil {
		t.Fatal("expected disconnected FS after dial failure")
	}
}

func TestProvisionSlotLRUSMBInvalidURL(t *testing.T) {
	m := ReasoningEffort{SlotSavePath: "smb://user@host"}
	if err := m.provisionSlotLRU(zap.NewNop()); err == nil {
		t.Fatal("expected error for smb URL without share, got nil")
	}
}

func TestProvisionSlotLRUSMBMissingUser(t *testing.T) {
	m := ReasoningEffort{SlotSavePath: "smb://host/share"}
	if err := m.provisionSlotLRU(zap.NewNop()); err == nil {
		t.Fatal("expected error for smb URL without username, got nil")
	}
}

func TestRedactSMBURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"smb://user:pass@host/share", "smb://***@host/share"},
		{"smb://domain;user:pass@host:1445/share", "smb://***@host:1445/share"},
		{"smb://host/share", "smb://host/share"},
		{"/var/lib/llama/slots", "/var/lib/llama/slots"},
	}
	for _, tt := range tests {
		if got := redactSMBURL(tt.in); got != tt.want {
			t.Errorf("redactSMBURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSMBFSPathJoining(t *testing.T) {
	if got := (&smbFS{cfg: &smbURL{}}).path("a.bin"); got != "a.bin" {
		t.Errorf("share root: got %q, want %q", got, "a.bin")
	}
	if got := (&smbFS{cfg: &smbURL{root: "slots"}}).path("a.bin"); got != "slots/a.bin" {
		t.Errorf("subdirectory: got %q, want %q", got, "slots/a.bin")
	}
}

func TestIsConnErr(t *testing.T) {
	if !isConnErr(&smb2.TransportError{Err: os.ErrClosed}) {
		t.Error("expected TransportError to be a connection error")
	}
	if !isConnErr(&smb2.ContextError{Err: context.DeadlineExceeded}) {
		t.Error("expected ContextError to be a connection error")
	}
	if !isConnErr(fmt.Errorf("wrap: %w", &smb2.TransportError{Err: os.ErrClosed})) {
		t.Error("expected wrapped TransportError to be a connection error")
	}
	if isConnErr(os.ErrNotExist) {
		t.Error("did not expect ErrNotExist to be a connection error")
	}
	if isConnErr(&os.PathError{Op: "open", Path: "x", Err: syscall.ENOENT}) {
		t.Error("did not expect PathError to be a connection error")
	}
	if isConnErr(nil) {
		t.Error("did not expect nil to be a connection error")
	}
}

// flakyFS is an in-memory SlotFS whose reads of the LRU state file fail
// until a limited number of attempts have been made, simulating an SMB
// connection that is down at startup and recovers later.
type flakyFS struct {
	inner *fakeFS
	mu    sync.Mutex
	fails int
	// onStateRead, if non-nil, is called after every read attempt of the
	// LRU state file, so tests can synchronize with the background load.
	onStateRead func()
}

func (f *flakyFS) Remove(name string) error       { return f.inner.Remove(name) }
func (f *flakyFS) Stat(name string) (bool, error) { return f.inner.Stat(name) }
func (f *flakyFS) WriteFile(name string, data []byte) error {
	return f.inner.WriteFile(name, data)
}
func (f *flakyFS) Rename(oldname, newname string) error {
	return f.inner.Rename(oldname, newname)
}
func (f *flakyFS) ListFiles() ([]SlotFileInfo, error) { return f.inner.ListFiles() }

func (f *flakyFS) ReadFile(name string) ([]byte, error) {
	if name != slotLRUStateFile {
		return f.inner.ReadFile(name)
	}
	f.mu.Lock()
	failed := f.fails > 0
	if failed {
		f.fails--
	}
	hook := f.onStateRead
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if failed {
		return nil, errors.New("smb connection broken")
	}
	return f.inner.ReadFile(name)
}

func TestSlotLRUDeferredInitRetriesOnFirstUse(t *testing.T) {
	fs := newFakeFS()
	fs.seed("a.bin", []byte("x"))
	state, _ := json.Marshal(slotLRUState{Entries: []string{"a.bin"}})
	fs.seed(slotLRUStateFile, state)
	attempted := make(chan struct{})
	var once sync.Once
	flaky := &flakyFS{
		inner:       fs,
		fails:       1,
		onStateRead: func() { once.Do(func() { close(attempted) }) },
	}

	l := newSlotLRU(flaky, 5, zap.NewNop())

	// Wait for the background load to make its (failing) attempt, so the
	// single failure is consumed by the background load, not the first use.
	<-attempted
	l.mu.Lock()
	uninitialized := l.index == nil
	l.mu.Unlock()
	if !uninitialized {
		t.Fatal("expected uninitialized table after failed background load")
	}

	// The first use retries the load and picks up the persisted state.
	l.record(saveBody("b.bin"))
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"b.bin", "a.bin"}) {
		t.Fatalf("order = %v, want [b.bin a.bin]", got)
	}
}

func TestSlotLRUStartsEmptyWhenLoadKeepsFailing(t *testing.T) {
	fs := newFakeFS()
	flaky := &flakyFS{inner: fs, fails: 1 << 30}

	l := newSlotLRU(flaky, 5, zap.NewNop())
	l.record(saveBody("a.bin"))
	if got := lruOrder(l); !reflect.DeepEqual(got, []string{"a.bin"}) {
		t.Fatalf("order = %v, want [a.bin]", got)
	}
}

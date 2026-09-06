package reasoningeffort

import (
	"container/list"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// slotLRUStateFile is the name (relative to the slot save directory) of
// the persisted LRU state. The leading dot keeps it out of the way of
// llama-server's slot save files.
const slotLRUStateFile = ".slot-lru.json"

// slotLRUStateFileTmpPrefix prefixes the temporary names used for atomic
// state writes. Each write gets a random suffix so concurrent writers
// (e.g. overlapping plugin instances during a Caddy reload) use distinct
// files and the last rename wins.
const slotLRUStateFileTmpPrefix = ".slot-lru.json.tmp-"

// newSlotLRUTmpName returns a unique temporary name for an atomic state
// write.
func newSlotLRUTmpName() (string, error) {
	var buf [4]byte
	if _, err := crand.Read(buf[:]); err != nil {
		return "", err
	}
	return slotLRUStateFileTmpPrefix + hex.EncodeToString(buf[:]), nil
}

// slotLRU is an LRU of slot save file names (relative to the slot save
// directory). The list head is the most recently used entry, the tail the
// least recently used. When the list grows past Max (a positive value),
// the least recently used entries are evicted and their files deleted.
//
// All exported methods are safe for concurrent use.
type slotLRU struct {
	mu    sync.Mutex
	list  *list.List // elements: string (file name)
	index map[string]*list.Element
	max   int // 0 = no limit, no eviction
	fs    SlotFS
	log   *zap.Logger
}

// slotLRUState is the on-disk shape of the LRU state: file names ordered
// from most recently used to least recently used.
type slotLRUState struct {
	Entries []string `json:"entries"`
}

// newSlotLRU creates an LRU backed by fs with the given entry limit (0 =
// no limit). It loads any previously persisted state, reconciles it
// against the files actually present on disk, and applies the limit if
// the persisted list is longer than max.
func newSlotLRU(fs SlotFS, max int, log *zap.Logger) *slotLRU {
	if log == nil {
		log = zap.NewNop()
	}
	l := &slotLRU{
		list:  list.New(),
		index: make(map[string]*list.Element),
		max:   max,
		fs:    fs,
		log:   log,
	}

	// Load the persisted state, if any. changed tracks whether the
	// in-memory table diverged from the file on disk, so the reconciled
	// state is written back only when needed.
	changed := false
	var state slotLRUState
	data, err := fs.ReadFile(slotLRUStateFile)
	if err != nil {
		if !isNotExist(err) {
			log.Warn("slot LRU: failed to read state file, starting empty",
				zap.String("file", slotLRUStateFile), zap.Error(err))
		}
	} else if err := json.Unmarshal(data, &state); err != nil {
		log.Warn("slot LRU: state file is corrupt, starting empty",
			zap.String("file", slotLRUStateFile), zap.Error(err))
		changed = true
	} else {
		for _, name := range state.Entries {
			if !isValidSlotFilename(name) {
				log.Warn("slot LRU: dropping invalid entry from state file",
					zap.String("filename", name))
				changed = true
				continue
			}
			if _, ok := l.index[name]; ok {
				// Duplicate in the state file; keep the first occurrence.
				continue
			}
			// Reconcile: drop entries whose file no longer exists on disk.
			exists, serr := fs.Stat(name)
			if serr != nil {
				log.Warn("slot LRU: failed to stat entry, dropping it",
					zap.String("filename", name), zap.Error(serr))
				changed = true
				continue
			}
			if !exists {
				log.Debug("slot LRU: dropping entry with no file on disk",
					zap.String("filename", name))
				changed = true
				continue
			}
			// The state file lists entries MRU first; appending in order keeps
			// the list head as the MRU entry.
			l.index[name] = l.list.PushBack(name)
		}
	}

	// Adopt files on disk that are not tracked yet (see adoptUntracked).
	if l.adoptUntracked() {
		changed = true
	}

	// The limit may have been lowered since the state was written.
	if l.evict() {
		changed = true
	}
	if changed {
		l.persist()
	}
	return l
}

// adoptUntracked scans the slot directory and adds files that are not yet
// tracked. They are appended after all tracked entries, ordered by
// modification time (newest first): untracked files are therefore always
// evicted before tracked ones, and the oldest untracked file is evicted
// first. The LRU state files are never adopted. It reports whether any
// file was adopted.
func (l *slotLRU) adoptUntracked() bool {
	files, err := l.fs.ListFiles()
	if err != nil {
		if isNotExist(err) {
			l.log.Warn("slot LRU: slot directory does not exist yet, skipping adoption")
		} else {
			l.log.Warn("slot LRU: failed to list slot directory, skipping adoption",
				zap.Error(err))
		}
		return false
	}
	var untracked []SlotFileInfo
	for _, f := range files {
		if !isValidSlotFilename(f.Name) {
			continue
		}
		if _, ok := l.index[f.Name]; ok {
			continue
		}
		untracked = append(untracked, f)
	}
	if len(untracked) == 0 {
		return false
	}
	// Newest first: entries are pushed back in MRU-first order, so the
	// oldest ends up at the list tail (LRU position).
	sort.Slice(untracked, func(i, j int) bool {
		if !untracked[i].ModTime.Equal(untracked[j].ModTime) {
			return untracked[i].ModTime.After(untracked[j].ModTime)
		}
		return untracked[i].Name < untracked[j].Name
	})
	for _, f := range untracked {
		l.index[f.Name] = l.list.PushBack(f.Name)
		l.log.Debug("slot LRU: adopted untracked file",
			zap.String("filename", f.Name), zap.Time("mod_time", f.ModTime))
	}
	l.log.Info("slot LRU: adopted untracked files", zap.Int("count", len(untracked)))
	return true
}

// record processes a save/restore request body and updates the LRU: the
// file named in the body becomes the most recently used entry. It is
// called asynchronously after the upstream request has completed.
func (l *slotLRU) record(body []byte) {
	var req slotRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		l.log.Warn("slot LRU: failed to parse request body", zap.Error(err))
		return
	}
	name := req.Filename
	if !isValidSlotFilename(name) {
		l.log.Warn("slot LRU: invalid filename in request body, ignoring",
			zap.String("filename", name))
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if el, ok := l.index[name]; ok {
		l.list.MoveToFront(el)
	} else {
		l.index[name] = l.list.PushFront(name)
		l.log.Debug("slot LRU: added entry", zap.String("filename", name))
	}
	l.evict()
	l.persist()
}

// delete removes the file with the given name and drops it from the LRU
// if present. It is idempotent: a missing file is not an error. It
// reports whether the file was actually deleted.
func (l *slotLRU) delete(name string) (bool, error) {
	if !isValidSlotFilename(name) {
		return false, fmt.Errorf("invalid slot filename %q", name)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	deleted := true
	if err := l.fs.Remove(name); err != nil {
		if isNotExist(err) {
			deleted = false
		} else {
			return false, err
		}
	}

	changed := false
	if el, ok := l.index[name]; ok {
		l.list.Remove(el)
		delete(l.index, name)
		changed = true
	}
	if changed {
		l.persist()
	}
	l.log.Debug("slot LRU: deleted entry",
		zap.String("filename", name), zap.Bool("file_deleted", deleted))
	return deleted, nil
}

// evict removes least recently used entries (and their files) until the
// list is within the limit, reporting whether anything was evicted.
// Callers must hold l.mu. A limit of 0 disables eviction.
func (l *slotLRU) evict() bool {
	evicted := false
	for l.max > 0 && l.list.Len() > l.max {
		back := l.list.Back()
		if back == nil {
			break
		}
		name := back.Value.(string)
		l.list.Remove(back)
		delete(l.index, name)
		if err := l.fs.Remove(name); err != nil {
			if !isNotExist(err) {
				l.log.Warn("slot LRU: failed to delete evicted file, entry still removed",
					zap.String("filename", name), zap.Error(err))
			}
		}
		l.log.Info("slot LRU: evicted least recently used entry",
			zap.String("filename", name))
		evicted = true
	}
	return evicted
}

// persist writes the LRU state (most recently used first) to the state
// file atomically: the data goes to a uniquely named temporary file that
// is renamed over the state file, so concurrent writers never share a
// temporary file and the last rename wins. Callers must hold l.mu.
func (l *slotLRU) persist() {
	entries := make([]string, 0, l.list.Len())
	for el := l.list.Front(); el != nil; el = el.Next() {
		entries = append(entries, el.Value.(string))
	}
	data, err := json.Marshal(slotLRUState{Entries: entries})
	if err != nil {
		l.log.Warn("slot LRU: failed to serialize state", zap.Error(err))
		return
	}
	tmp, err := newSlotLRUTmpName()
	if err != nil {
		l.log.Warn("slot LRU: failed to generate temporary state file name",
			zap.Error(err))
		return
	}
	if err := l.fs.WriteFile(tmp, data); err != nil {
		l.log.Warn("slot LRU: failed to write state file",
			zap.String("file", tmp), zap.Error(err))
		return
	}
	if err := l.fs.Rename(tmp, slotLRUStateFile); err != nil {
		// Best-effort cleanup of the orphaned temporary file.
		if rerr := l.fs.Remove(tmp); rerr != nil && !isNotExist(rerr) {
			l.log.Warn("slot LRU: failed to remove temporary state file",
				zap.String("file", tmp), zap.Error(rerr))
		}
		l.log.Warn("slot LRU: failed to replace state file",
			zap.String("file", slotLRUStateFile), zap.Error(err))
	}
}

// isValidSlotFilename reports whether name is a usable slot file name:
// non-empty, no path separators, no dot entries, and not one of the
// reserved LRU state file names.
func isValidSlotFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if name == slotLRUStateFile || strings.HasPrefix(name, slotLRUStateFileTmpPrefix) {
		return false
	}
	return true
}

// isNotExist reports whether err indicates a missing file. SlotFS
// implementations must wrap os.ErrNotExist for missing files (see the
// SlotFS docs), so errors.Is is sufficient.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

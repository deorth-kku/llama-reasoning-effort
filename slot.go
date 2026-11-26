package reasoningeffort

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// slotsPathPrefix is the request path prefix of llama-server's
// /slots/{id_slot} management endpoint.
const slotsPathPrefix = "/slots/"

// maxSlotRequestBody caps the size of the request body read for the
// delete action. The body only carries a filename, and OS file names
// are short.
const maxSlotRequestBody = 512

// slotRequestBody is the JSON body of a /slots/{id_slot} save/restore/
// delete request; only the filename field matters to this plugin.
type slotRequestBody struct {
	Filename string `json:"filename,omitzero"`
}

// newSlotFS creates the SlotFS for path: an smb:// URL uses an SMB
// share (via the pure-Go go-smb2 client), an nfs:// URL an NFSv4 export
// (via the pure-Go go-nfs-client), anything else a local directory. The
// returned closer is nil for local directories and releases the SMB
// session or NFS connection otherwise. A failed SMB or NFS connection
// is not an error: the share/export is reached on first use instead.
func newSlotFS(path string, log *zap.Logger) (SlotFS, io.Closer, error) {
	if strings.HasPrefix(path, "smb://") {
		fs, err := newSMBFSFromURL(path, log)
		if err != nil {
			return nil, nil, fmt.Errorf("slot_save_path: %w", err)
		}
		return fs, fs, nil
	}
	if strings.HasPrefix(path, "nfs://") {
		fs, err := newNFSFSFromURL(path, log)
		if err != nil {
			return nil, nil, fmt.Errorf("slot_save_path: %w", err)
		}
		return fs, fs, nil
	}
	// Do not block startup when the directory is missing: llama-server
	// may create it later, and the LRU load reports it and reconciles
	// against disk.
	return newLocalFS(path), nil, nil
}

// provisionSlotLRU initializes the handler-level slot file LRU from the
// configured slot options. It returns an error when the options are
// half-configured (a limit without a save path) or the SMB/NFS URL is
// malformed. In terminating mode the instance is additionally shared
// by every upstream that has no slot settings of its own.
func (m *ReasoningEffort) provisionSlotLRU(log *zap.Logger) error {
	if m.SlotLRUMax < 0 {
		return fmt.Errorf("slot_lru_max must be >= 0, got %d", m.SlotLRUMax)
	}
	if m.SlotSavePath == "" {
		if m.SlotLRUMax != 0 {
			return fmt.Errorf("slot_lru_max is set but slot_save_path is not")
		}
		return nil
	}
	fs, closer, err := newSlotFS(m.SlotSavePath, log)
	if err != nil {
		return err
	}
	if closer != nil {
		m.fsClosers = append(m.fsClosers, closer)
	}
	m.slotLRU = newSlotLRU(fs, m.SlotLRUMax, log)
	return nil
}

// Cleanup implements caddy.CleanerUpper and releases the SMB sessions
// and NFS connections used by the slot features.
func (m *ReasoningEffort) Cleanup() error {
	var err error
	for _, c := range m.fsClosers {
		if e := c.Close(); e != nil && err == nil {
			err = e
		}
	}
	m.fsClosers = nil
	return err
}

// serveSlots handles POST requests to llama-server's /slots/{id_slot}
// endpoint, using lru as the slot file LRU of the directory the request
// is forwarded to (the handler-level LRU in middleware mode, the
// selected upstream's LRU in terminating mode). It returns handled=true
// when the request was fully processed by this method (either forwarded
// to next or answered directly), and handled=false when the request
// should be passed through untouched.
//
// save/restore are forwarded to the upstream immediately with the body
// tee'd to a local copy; the LRU is updated asynchronously from that copy
// after next returns. delete is answered by this plugin directly, since
// llama-server has no delete action (it would answer 400 "Invalid
// action"); the slot id in the path is ignored — delete targets a file
// only.
func (m *ReasoningEffort) serveSlots(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler, lru *slotLRU) (bool, error) {
	if lru == nil || r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, slotsPathPrefix) {
		return false, nil
	}
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	action := r.URL.Query().Get("action")
	switch action {
	case "delete":
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSlotRequestBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return true, nil
		}
		var req slotRequestBody
		if err := json.Unmarshal(body, &req); err != nil || !isValidSlotFilename(req.Filename) {
			http.Error(w, "invalid filename", http.StatusBadRequest)
			return true, nil
		}
		deleted, err := lru.delete(req.Filename)
		if err != nil {
			log.Warn("slot delete failed", zap.String("filename", req.Filename), zap.Error(err))
			http.Error(w, "failed to delete slot file", http.StatusInternalServerError)
			return true, nil
		}
		resp, _ := json.Marshal(map[string]any{"filename": req.Filename, "deleted": deleted})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp)
		return true, nil
	case "save", "restore":
		// Tee the body to a local copy while forwarding it immediately.
		// The LRU is updated from the copy after next returns: by then the
		// upstream has fully read the request body, so the copy is
		// complete (TeeReader and bytes.Buffer are not safe for concurrent
		// reads, so the copy must not be read while next is running).
		bodyCopy := &bytes.Buffer{}
		orig := r.Body
		r.Body = &teeCloser{Reader: io.TeeReader(orig, bodyCopy), closer: orig}
		err := next.ServeHTTP(w, r)
		go lru.record(bodyCopy.Bytes())
		return true, err
	default:
		// erase and anything else: pass through untouched.
		return false, nil
	}
}

// teeCloser pairs a reader (typically a TeeReader) with the Close of the
// underlying request body, so wrapping r.Body does not leak the original
// connection cleanup.
type teeCloser struct {
	io.Reader
	closer io.Closer
}

func (t *teeCloser) Close() error { return t.closer.Close() }

// parseSlotOption parses the slot_save_path and slot_lru_max directives
// into *path and *max. The dispenser must be positioned on the directive
// token.
func parseSlotOption(d *caddyfile.Dispenser, path *string, max *int) error {
	switch d.Val() {
	case "slot_save_path":
		if !d.NextArg() {
			return d.ArgErr()
		}
		*path = d.Val()
	case "slot_lru_max":
		if !d.NextArg() {
			return d.ArgErr()
		}
		val, err := strconv.Atoi(d.Val())
		if err != nil {
			return d.Errf("invalid slot_lru_max value '%s': %v", d.Val(), err)
		}
		*max = val
	}
	return nil
}

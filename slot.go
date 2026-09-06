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

// provisionSlotLRU initializes the slot file LRU from the configured
// slot options. It returns an error when the options are half-configured
// (a limit without a save path).
func (m *ReasoningEffort) provisionSlotLRU(log *zap.Logger) error {
	switch {
	case m.SlotSavePath != "":
		if m.SlotLRUMax < 0 {
			return fmt.Errorf("slot_lru_max must be >= 0, got %d", m.SlotLRUMax)
		}
		// Do not block startup when the directory is missing: llama-server
		// may create it later, and the LRU load reports it and reconciles
		// against disk.
		m.slotLRU = newSlotLRU(newLocalFS(m.SlotSavePath), m.SlotLRUMax, log)
	case m.SlotLRUMax != 0:
		return fmt.Errorf("slot_lru_max is set but slot_save_path is not")
	}
	return nil
}

// serveSlots handles POST requests to llama-server's /slots/{id_slot}
// endpoint. It returns handled=true when the request was fully processed
// by this method (either forwarded to next or answered directly), and
// handled=false when the request should be passed through untouched.
//
// save/restore are forwarded to the upstream immediately with the body
// tee'd to a local copy; the LRU is updated asynchronously from that copy
// after next returns. delete is answered by this plugin directly, since
// llama-server has no delete action (it would answer 400 "Invalid
// action"); the slot id in the path is ignored — delete targets a file
// only.
func (m *ReasoningEffort) serveSlots(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) (bool, error) {
	if m.slotLRU == nil || r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, slotsPathPrefix) {
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
		deleted, err := m.slotLRU.delete(req.Filename)
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
		go m.slotLRU.record(bodyCopy.Bytes())
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

// parseSlotOption parses the slot_save_path and slot_lru_max directives.
// The dispenser must be positioned on the directive token.
func (m *ReasoningEffort) parseSlotOption(d *caddyfile.Dispenser) error {
	switch d.Val() {
	case "slot_save_path":
		if !d.NextArg() {
			return d.ArgErr()
		}
		m.SlotSavePath = d.Val()
	case "slot_lru_max":
		if !d.NextArg() {
			return d.ArgErr()
		}
		val, err := strconv.Atoi(d.Val())
		if err != nil {
			return d.Errf("invalid slot_lru_max value '%s': %v", d.Val(), err)
		}
		m.SlotLRUMax = val
	}
	return nil
}

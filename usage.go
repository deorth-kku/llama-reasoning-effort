package reasoningeffort

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"slices"
	"sync"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// usageChunk mirrors the fields of a streaming chat completion chunk that we
// care about. Only `usage` and `timings` are captured; every other field (id,
// object, choices, ...) is ignored during unmarshaling. Both are maps so that
// any fields the server includes (which vary by implementation) are preserved.
type usageChunk struct {
	Usage   map[string]any `json:"usage,omitzero"`
	Timings map[string]any `json:"timings,omitzero"`
}

// sseUsageParser incrementally parses an SSE stream, extracting the usage and
// timings objects from each complete `data:` event and discarding the event
// once it is processed. It retains only the trailing bytes of a not-yet-
// terminated event, so memory usage stays bounded by the size of a single SSE
// event rather than the whole stream.
type sseUsageParser struct {
	pending []byte         // trailing bytes of an incomplete event
	usage   map[string]any // last usage object seen
	timings map[string]any // last timings object seen
}

// feed appends a chunk of SSE bytes and processes every complete event it
// contains, discarding each as it goes. It returns the last usage seen so far
// (nil when none has been seen).
func (p *sseUsageParser) feed(chunk []byte) map[string]any {
	p.pending = append(p.pending, chunk...)
	// SSE events are separated by a blank line (\n\n). Process each complete
	// event and drop it, keeping only the trailing partial event.
	for {
		idx := bytes.Index(p.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		event := p.pending[:idx]
		p.pending = slices.Clone(p.pending[idx+2:])
		p.processEvent(event)
	}
	return p.usage
}

// finalize processes any trailing partial event left over once the stream has
// ended (the final event may not be terminated by a blank line) and returns
// the last usage seen, or nil.
func (p *sseUsageParser) finalize() map[string]any {
	if len(p.pending) > 0 {
		p.processEvent(p.pending)
		p.pending = nil
	}
	return p.usage
}

// processEvent scans a single SSE event's lines for a `data:` payload and
// records its usage object when present. The terminal `data: [DONE]` line and
// non-JSON payloads are ignored.
func (p *sseUsageParser) processEvent(event []byte) {
	for line := range bytes.SplitSeq(event, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var chunk usageChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			p.usage = chunk.Usage
		}
		if chunk.Timings != nil {
			p.timings = chunk.Timings
		}
	}
}

// teeBufferChunks is the capacity of the streamTeeWriter's byte channel. It
// is large enough that the write path almost never blocks; when it does fill
// up, blocking is acceptable backpressure.
const teeBufferChunks = 256

// streamTeeWriter wraps an http.ResponseWriter, forwarding every write to the
// underlying writer while handing the bytes to a background goroutine that
// runs the incremental SSE usage parser. Writes reach the real writer
// immediately, so streaming to the client is never delayed by parsing. The
// bytes are copied into a buffered channel; the reader goroutine consumes the
// channel and parses. Closing the channel is the termination signal.
type streamTeeWriter struct {
	http.ResponseWriter
	ch      chan []byte
	wg      sync.WaitGroup
	usage   map[string]any
	timings map[string]any
}

func newStreamTeeWriter(w http.ResponseWriter) *streamTeeWriter {
	tw := &streamTeeWriter{
		ResponseWriter: w,
		ch:             make(chan []byte, teeBufferChunks),
	}
	tw.wg.Go(tw.readLoop)
	return tw
}

// readLoop drains the byte channel, feeding each chunk to a private parser,
// and finalizes once the channel is closed. It records the final usage and
// signals completion so the caller can wait for it.
func (t *streamTeeWriter) readLoop() {
	p := &sseUsageParser{}
	for chunk := range t.ch {
		p.feed(chunk)
	}
	t.usage = p.finalize()
	t.timings = p.timings
}

// Write forwards the bytes to the real writer first so the client is never
// delayed, then hands a copy to the parser goroutine. The copy is required
// because the caller may reuse its buffer after Write returns. The send
// blocks only if the buffer is full (backpressure).
func (t *streamTeeWriter) Write(p []byte) (int, error) {
	n, err := t.ResponseWriter.Write(p)
	if n > 0 {
		t.ch <- slices.Clone(p[:n])
	}
	return n, err
}

type Unwraper interface {
	Unwrap() http.ResponseWriter
}

var _ Unwraper = (*streamTeeWriter)(nil)

// Unwrap returns the underlying ResponseWriter so that
// http.ResponseController can see through this wrapper and find
// optional methods (Flush, FlushError, Hijack, ...) on the concrete
// writer. Without it, NewResponseController(tw).Flush is a silent
// no-op, because the embedded interface exposes no Flush method.
func (t *streamTeeWriter) Unwrap() http.ResponseWriter {
	if un, ok := t.ResponseWriter.(Unwraper); ok {
		return un.Unwrap()
	}
	return t.ResponseWriter
}

// teeReader wraps an io.Reader, copying each Read result to a channel
// before returning it. This allows the underlying ResponseWriter's
// ReadFrom to receive the bytes while the parser goroutine also gets them.
type teeReader struct {
	r  io.Reader
	ch chan []byte
}

func (tr *teeReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	if n > 0 {
		tr.ch <- slices.Clone(p[:n])
	}
	return n, err
}

// finish closes the byte channel to signal the reader goroutine to drain the
// remaining buffered bytes and finalize, then waits for it to finish.
func (t *streamTeeWriter) finish() {
	close(t.ch)
	t.wg.Wait()
}

// serveChatCompletions runs the downstream handler for a /v1/chat/completions
// request with a response writer that tees the SSE stream into an incremental
// usage parser. Once the stream completes, the usage object is logged. The
// client has already received the full response by that point, so logging
// never delays it.
func (m ReasoningEffort) serveChatCompletions(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler, model string) error {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	tw := newStreamTeeWriter(w)
	if err := next.ServeHTTP(tw, r); err != nil {
		tw.finish()
		return err
	}

	// The stream is complete. Signal the reader goroutine to drain the
	// remaining buffered bytes, finalize, and log the usage. The response has
	// already been fully sent to the client, so this never delays it.
	tw.finish()
	if u := tw.usage; u != nil {
		log.Info("chat completions usage",
			zap.String("model", model),
			zap.Any("usage", u),
			zap.Any("timings", tw.timings),
		)
	} else {
		log.Debug("no usage found in chat completions stream", zap.String("model", model))
	}
	return nil
}

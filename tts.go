package reasoningeffort

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	cjson "github.com/deorth-kku/go-common/json"
	"go.uber.org/zap"
)

// provisionVoices reads every configured voice reference file and caches
// its base64 encoding in the config's voiceCache, so request handling
// performs no disk I/O. A missing or unreadable file is a startup error.
func (m *ReasoningEffort) provisionVoices(log *zap.Logger) error {
	configs := []struct {
		name string
		cfg  *ModelConfig
	}{
		{"", &m.ModelConfig},
	}
	for name, mc := range m.ModelConfigs {
		configs = append(configs, struct {
			name string
			cfg  *ModelConfig
		}{name, &mc})
	}
	for _, c := range configs {
		for voice, path := range c.cfg.Voices {
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("voices[%q] (model %q): %w", voice, c.name, err)
			}
			if c.cfg.voiceCache == nil {
				c.cfg.voiceCache = map[string]string{}
			}
			c.cfg.voiceCache[voice] = base64.StdEncoding.EncodeToString(data)
			log.Debug("cached voice reference",
				zap.String("model", c.name),
				zap.String("voice", voice),
				zap.Int("bytes", len(data)))
		}
	}
	return nil
}

// ttsRequestBody is the OpenAI /v1/audio/speech JSON shape. Fields defined
// explicitly are accessed by name; everything else (speed, unknown fields)
// is captured via Inline and passed through to the backend unchanged.
type ttsRequestBody struct {
	Model          string         `json:"model,omitzero"`
	Input          string         `json:"input,omitzero"`
	Voice          string         `json:"voice,omitzero"`
	ResponseFormat string         `json:"response_format,omitzero"`
	Stream         *bool          `json:"stream,omitzero"`
	SpeakerRefB64  string         `json:"speaker_ref_b64,omitzero"`
	Inline         jsontext.Value `json:",embed"`
}

// serveAudioSpeech rewrites an OpenAI /v1/audio/speech request into
// llama.cpp /tts shape and forwards it. The voice is mapped to a local
// speaker reference file (cached as base64 in Provision) and sent as
// speaker_ref_b64. A pcm response_format additionally forces stream=true
// on the request, and the backend's float32 PCM response is converted to
// int16 on the fly.
func (m ReasoningEffort) serveAudioSpeech(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	log := m.log
	if log == nil {
		log = zap.NewNop()
	}

	// Read and preserve the body.
	bodyCopy := bytes.Buffer{}
	tee := io.TeeReader(r.Body, &bodyCopy)

	var body ttsRequestBody
	if r.ContentLength > 0 {
		body.Inline = make(jsontext.Value, 0, min(r.ContentLength, max_allocate_len))
	}
	if err := json.UnmarshalRead(tee, &body); err != nil {
		log.Warn("audio speech: body is not valid request, forwarding unchanged", zap.Error(err))
		r.Body = io.NopCloser(&bodyCopy)
		r.URL.Path = m.TTSPath
		return next.ServeHTTP(w, r)
	}

	// Normalize the response format. OpenAI's default is mp3, which the
	// backend cannot produce; default to wav instead.
	switch body.ResponseFormat {
	case "":
		body.ResponseFormat = "wav"
	case "wav", "pcm":
	default:
		return writeAudioSpeechError(w, http.StatusBadRequest,
			fmt.Sprintf("unsupported response_format %q: only \"wav\" and \"pcm\" are supported", body.ResponseFormat))
	}

	// Resolve the model config by the request's model field.
	useconfig := m.ModelConfig
	if mc, ok := m.ModelConfigs[body.Model]; ok {
		useconfig = mc
	}

	// Map the voice to a cached speaker reference. Voices absent from the
	// map are passed through without a reference (model-native voices).
	if ref, ok := useconfig.voiceCache[body.Voice]; ok {
		body.SpeakerRefB64 = ref
	}

	// The backend only streams pcm when asked to; pcm clients expect a
	// stream, so force it.
	if body.ResponseFormat == "pcm" {
		stream := true
		body.Stream = &stream
	}

	r.Body = io.NopCloser(cjson.NewJsonReader(body))
	// The backend accepts an unknown content length, so we can avoid
	// allocating the full re-serialized body as a byte slice.
	r.ContentLength = -1
	r.Header.Del("Content-Length")
	r.URL.Path = m.TTSPath

	if body.ResponseFormat == "pcm" {
		return next.ServeHTTP(newPcmResponseWriter(w), r)
	}
	return next.ServeHTTP(w, r)
}

// writeAudioSpeechError writes an OpenAI-style JSON error response.
func writeAudioSpeechError(w http.ResponseWriter, status int, message string) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.MarshalWrite(w, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

// pcmResponseWriter wraps the response writer for pcm requests. It
// captures the upstream headers and status, then on the first body write
// commits them with the Content-Type rewritten from float32 to int16 PCM
// and streams the body through a float32→int16 converter. Each upstream
// chunk is converted and written to the client immediately; the response
// is never buffered in full. Non-200 responses (JSON errors from the
// backend) pass through unconverted.
type pcmResponseWriter struct {
	http.ResponseWriter
	header     http.Header
	statusCode int
	committed  bool
	// carry holds the 0..3 leftover bytes of an incomplete float32 frame
	// from the previous chunk.
	carry    [3]byte
	carryLen int
	// buf and out are reused scratch buffers.
	buf []byte
	out []byte
}

var _ Unwraper = (*pcmResponseWriter)(nil)

func newPcmResponseWriter(w http.ResponseWriter) *pcmResponseWriter {
	h := make(http.Header, len(w.Header()))
	for k, vv := range w.Header() {
		h[k] = append([]string(nil), vv...)
	}
	return &pcmResponseWriter{
		ResponseWriter: w,
		header:         h,
		statusCode:     http.StatusOK,
	}
}

// Header returns the captured header map so upstream writes are recorded
// here instead of on the real writer.
func (p *pcmResponseWriter) Header() http.Header { return p.header }

func (p *pcmResponseWriter) WriteHeader(code int) {
	if !p.committed {
		p.statusCode = code
	}
}

func (p *pcmResponseWriter) Unwrap() http.ResponseWriter {
	if un, ok := p.ResponseWriter.(Unwraper); ok {
		return un.Unwrap()
	}
	return p.ResponseWriter
}

// Write commits the captured headers on the first call, then converts the
// float32 LE samples in p to int16 LE and writes them to the real writer.
func (p *pcmResponseWriter) Write(src []byte) (int, error) {
	if !p.committed {
		p.commit()
	}
	if p.statusCode != http.StatusOK {
		// Upstream error bodies (JSON) pass through untouched.
		return p.ResponseWriter.Write(src)
	}
	if len(src) == 0 {
		return 0, nil
	}

	// Combine the carry-over bytes with the new chunk.
	in := p.buf
	if cap(in) < p.carryLen+len(src) {
		in = make([]byte, 0, p.carryLen+len(src))
	}
	in = append(in[:p.carryLen], src...)

	// Convert every complete 4-byte float32 frame to 2-byte int16.
	if cap(p.out) < len(in)/2+1 {
		p.out = make([]byte, 0, len(in)/2+1)
	}
	out := p.out[:0]
	for len(in) >= 4 {
		f := math.Float32frombits(binary.LittleEndian.Uint32(in[:4]))
		out = binary.LittleEndian.AppendUint16(out, uint16(s16FromF32(f)))
		in = in[4:]
	}
	// Keep the trailing 0..3 bytes for the next chunk.
	p.carryLen = copy(p.carry[:], in)

	// Write the converted bytes, retrying short writes.
	var written int
	for written < len(out) {
		w, err := p.ResponseWriter.Write(out[written:])
		written += w
		if err != nil {
			return 0, err
		}
	}
	// The writer contract is about input consumed, not output produced:
	// every input byte was converted, so report the full input length
	// even though the output is half as large.
	return len(src), nil
}

// commit writes the captured status and headers to the real writer. The
// Content-Type is rewritten from float32 to int16 PCM (the sample rate is
// preserved) and length-framing headers are dropped: the converted body is
// half the upstream length, so net/http must re-frame it.
func (p *pcmResponseWriter) commit() {
	h := p.ResponseWriter.Header()
	for k, vv := range p.header {
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	if ct := h.Get("Content-Type"); ct != "" {
		if mt, param, err := mime.ParseMediaType(ct); err == nil && mt == "audio/pcm" {
			param["encoding"] = "int"
			param["bits"] = "16"
			h.Set("Content-Type", mime.FormatMediaType(mt, param))
		}
	}
	h.Del("Content-Length")
	h.Del("Transfer-Encoding")
	p.ResponseWriter.WriteHeader(p.statusCode)
	p.committed = true
}

// s16FromF32 converts a float32 sample in [-1, 1] to a 16-bit integer,
// clamping out-of-range values.
func s16FromF32(f float32) int16 {
	switch {
	case f >= 1:
		return 32767
	case f <= -1:
		return -32768
	default:
		return int16(f * 32768)
	}
}

package reasoningeffort

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// writeVoiceFile writes a synthetic speaker reference file into a temp
// directory and returns its path and content.
func writeVoiceFile(t *testing.T, name string, data []byte) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing voice file: %v", err)
	}
	return path, data
}

// provisionTestVoices builds a ModelConfig with the given voice→file map
// and a populated voiceCache, using the same code path as Provision.
func provisionTestVoices(t *testing.T, voices map[string]string) ModelConfig {
	t.Helper()
	m := ReasoningEffort{ModelConfig: ModelConfig{Voices: voices}}
	if err := m.provisionVoices(zap.NewNop()); err != nil {
		t.Fatalf("provisionVoices: %v", err)
	}
	return m.ModelConfig
}

// f32LE encodes float32 samples as little-endian bytes.
func f32LE(vals ...float32) []byte {
	var b []byte
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v))
	}
	return b
}

// s16LE encodes int16 samples as little-endian bytes.
func s16LE(vals ...int16) []byte {
	var b []byte
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	return b
}

// runAudioSpeech runs the middleware for an /v1/audio/speech request with
// the given upstream handler and returns the response recorder.
func runAudioSpeech(t *testing.T, m ReasoningEffort, body string, upstream func(w http.ResponseWriter)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, defaultAudioSpeechPath, bytes.NewBufferString(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	rec := httptest.NewRecorder()
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
		upstream(w)
		return nil
	})
	if err := m.ServeHTTP(rec, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return rec
}

func TestAudioSpeechRequestRewritePcm(t *testing.T) {
	ref := []byte{0xde, 0xad, 0xbe, 0xef}
	path, _ := writeVoiceFile(t, "ref-a.wav", ref)
	cfg := provisionTestVoices(t, map[string]string{"voice-a": path})

	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
		ModelConfig:     cfg,
	}
	cap := runHandler(t, m, `{"model":"model-x","input":"Hello, this is a test.","voice":"voice-a","response_format":"pcm"}`, defaultAudioSpeechPath)

	if cap.path != defaultTTSPath {
		t.Errorf("expected downstream path %q, got %q", defaultTTSPath, cap.path)
	}
	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["input"] != "Hello, this is a test." {
		t.Errorf("expected input preserved, got %v", got["input"])
	}
	if got["voice"] != "voice-a" {
		t.Errorf("expected voice preserved, got %v", got["voice"])
	}
	if got["response_format"] != "pcm" {
		t.Errorf("expected response_format pcm, got %v", got["response_format"])
	}
	if got["stream"] != true {
		t.Errorf("expected stream=true for pcm, got %v", got["stream"])
	}
	want := base64.StdEncoding.EncodeToString(ref)
	if got["speaker_ref_b64"] != want {
		t.Errorf("expected speaker_ref_b64 %q, got %v", want, got["speaker_ref_b64"])
	}
}

func TestAudioSpeechWavNoStream(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	cap := runHandler(t, m, `{"input":"hi","response_format":"wav"}`, defaultAudioSpeechPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["response_format"] != "wav" {
		t.Errorf("expected response_format wav, got %v", got["response_format"])
	}
	if _, ok := got["stream"]; ok {
		t.Errorf("expected stream absent for wav, got %v", got["stream"])
	}
	if _, ok := got["speaker_ref_b64"]; ok {
		t.Errorf("expected speaker_ref_b64 absent, got %v", got["speaker_ref_b64"])
	}
}

func TestAudioSpeechDefaultFormatWav(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	cap := runHandler(t, m, `{"input":"hi"}`, defaultAudioSpeechPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["response_format"] != "wav" {
		t.Errorf("expected default response_format wav, got %v", got["response_format"])
	}
}

func TestAudioSpeechUnsupportedFormat(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	called := false
	rec := runAudioSpeech(t, m, `{"input":"hi","response_format":"mp3"}`, func(w http.ResponseWriter) {
		called = true
	})
	if called {
		t.Error("upstream should not be called for an unsupported format")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("error body not valid json: %v", err)
	}
	errObj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error object, got %v", got)
	}
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %v", errObj["type"])
	}
}

func TestAudioSpeechUnmappedVoicePassesThrough(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	cap := runHandler(t, m, `{"input":"hi","voice":"female"}`, defaultAudioSpeechPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	if got["voice"] != "female" {
		t.Errorf("expected voice forwarded, got %v", got["voice"])
	}
	if _, ok := got["speaker_ref_b64"]; ok {
		t.Errorf("expected speaker_ref_b64 absent for unmapped voice, got %v", got["speaker_ref_b64"])
	}
}

func TestAudioSpeechVoicePerModelConfig(t *testing.T) {
	topRef := []byte{0x01, 0x02}
	topPath, _ := writeVoiceFile(t, "ref-top.wav", topRef)
	perRef := []byte{0x03, 0x04}
	perPath, _ := writeVoiceFile(t, "ref-per.wav", perRef)

	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
		ModelConfig:     provisionTestVoices(t, map[string]string{"voice-a": topPath}),
		ModelConfigs: map[string]ModelConfig{
			"model-x": provisionTestVoices(t, map[string]string{"voice-a": perPath}),
		},
	}
	cap := runHandler(t, m, `{"model":"model-x","input":"hi","voice":"voice-a"}`, defaultAudioSpeechPath)

	var got map[string]any
	if err := json.Unmarshal(cap.body, &got); err != nil {
		t.Fatalf("downstream body not valid json: %v", err)
	}
	want := base64.StdEncoding.EncodeToString(perRef)
	if got["speaker_ref_b64"] != want {
		t.Errorf("expected per-model speaker_ref_b64 %q, got %v", want, got["speaker_ref_b64"])
	}
}

func TestAudioSpeechInvalidJSONForwarded(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	cap := runHandler(t, m, `{not valid json`, defaultAudioSpeechPath)

	if cap.path != defaultTTSPath {
		t.Errorf("expected downstream path %q, got %q", defaultTTSPath, cap.path)
	}
	if string(cap.body) != `{not valid json` {
		t.Errorf("expected body forwarded unchanged, got %q", cap.body)
	}
}

func TestAudioSpeechWavPassthrough(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	wav := []byte{0x52, 0x49, 0x46, 0x46, 1, 2, 3, 4}
	rec := runAudioSpeech(t, m, `{"input":"hi"}`, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "audio/wav")
		if _, err := w.Write(wav); err != nil {
			t.Errorf("upstream write: %v", err)
		}
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("expected Content-Type audio/wav, got %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Errorf("expected wav bytes passed through unchanged, got %x", rec.Body.Bytes())
	}
}

func TestAudioSpeechPcmConversion(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	// Frame-aligned float32 samples, written in awkward chunk sizes to
	// exercise the carry-over handling across chunk boundaries.
	samples := []float32{0, 0.5, -0.5, 1, -1, 0.25, -0.25}
	data := f32LE(samples...)
	rec := runAudioSpeech(t, m, `{"input":"hi","response_format":"pcm"}`, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "audio/pcm;rate=24000;encoding=float;bits=32")
		// Write in awkward chunk sizes to exercise the carry-over
		// handling across chunk boundaries. The writer must report the
		// input length consumed (not the shorter output length) so that
		// io.Copy-style callers do not see a short write.
		for _, n := range []int{1, 3, 6, 7, 11} {
			if n > len(data) {
				n = len(data)
			}
			written, err := w.Write(data[:n])
			if err != nil {
				t.Errorf("upstream write: %v", err)
			}
			if written != n {
				t.Errorf("expected writer to consume %d input bytes, got %d", n, written)
			}
			data = data[n:]
		}
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/pcm; bits=16; encoding=int; rate=24000" {
		t.Errorf("expected Content-Type audio/pcm; bits=16; encoding=int; rate=24000, got %q", ct)
	}
	want := s16LE(0, 16384, -16384, 32767, -32768, 8192, -8192)
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("expected s16 bytes %x, got %x", want, rec.Body.Bytes())
	}
}

func TestAudioSpeechPcmConversionIoCopy(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	data := f32LE(0.1, -0.2, 0.3, -0.4)
	rec := runAudioSpeech(t, m, `{"input":"hi","response_format":"pcm"}`, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "audio/pcm;rate=24000;encoding=float;bits=32")
		// io.Copy fails with a short write unless the converter reports
		// the full input length consumed.
		if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
			t.Errorf("upstream io.Copy: %v", err)
		}
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	want := s16LE(3277, -6554, 9830, -13107)
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("expected s16 bytes %x, got %x", want, rec.Body.Bytes())
	}
}

func TestAudioSpeechPcmErrorPassthrough(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	errBody := []byte(`{"error":"model not loaded"}`)
	rec := runAudioSpeech(t, m, `{"input":"hi","response_format":"pcm"}`, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write(errBody); err != nil {
			t.Errorf("upstream write: %v", err)
		}
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), errBody) {
		t.Errorf("expected error body passed through unconverted, got %q", rec.Body.Bytes())
	}
}

func TestAudioSpeechPcmHeaderOnlyError(t *testing.T) {
	m := ReasoningEffort{
		AudioSpeechPath: defaultAudioSpeechPath,
		TTSPath:         defaultTTSPath,
	}
	rec := runAudioSpeech(t, m, `{"input":"hi","response_format":"pcm"}`, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		// No body is written: the status and headers must still reach the
		// client via the early commit on a non-200 WriteHeader.
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestPcmResponseWriterNoHeaderDuplication(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Existing", "a")
	w := newPcmResponseWriter(rec)
	w.Header().Set("Content-Type", "audio/pcm;rate=24000;encoding=float;bits=32")
	if _, err := w.Write(f32LE(0.5)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := rec.Header().Values("X-Existing"); len(got) != 1 || got[0] != "a" {
		t.Errorf("expected X-Existing to appear exactly once as %q, got %v", "a", got)
	}
}

func TestProvisionVoiceCache(t *testing.T) {
	ref := []byte{0x01, 0x02, 0x03}
	path, data := writeVoiceFile(t, "ref-a.wav", ref)
	m := ReasoningEffort{ModelConfig: ModelConfig{Voices: map[string]string{"voice-a": path}}}
	if err := m.provisionVoices(zap.NewNop()); err != nil {
		t.Fatalf("provisionVoices: %v", err)
	}
	want := base64.StdEncoding.EncodeToString(data)
	if got := m.ModelConfig.voiceCache["voice-a"]; got != want {
		t.Errorf("expected cached base64 %q, got %q", want, got)
	}
}

func TestProvisionVoiceCacheMissingFile(t *testing.T) {
	m := ReasoningEffort{
		ModelConfig: ModelConfig{Voices: map[string]string{"voice-a": filepath.Join(t.TempDir(), "absent.wav")}},
	}
	if err := m.provisionVoices(zap.NewNop()); err == nil {
		t.Fatal("expected an error for a missing voice file, got nil")
	}
}

func TestUnmarshalCaddyfileTTSPaths(t *testing.T) {
	input := `reasoning_effort {
		audio_speech_path /openai/audio/speech
		tts_path /backend/tts
	}`
	m := ReasoningEffort{}
	d := caddyfile.NewTestDispenser(input)
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if m.AudioSpeechPath != "/openai/audio/speech" {
		t.Errorf("expected AudioSpeechPath /openai/audio/speech, got %q", m.AudioSpeechPath)
	}
	if m.TTSPath != "/backend/tts" {
		t.Errorf("expected TTSPath /backend/tts, got %q", m.TTSPath)
	}
}

func TestUnmarshalCaddyfileVoice(t *testing.T) {
	input := `reasoning_effort {
		voice voice-a /tmp/ref-a.wav
		model model-x {
			voice voice-b /tmp/ref-b.wav
		}
	}`
	m := ReasoningEffort{}
	d := caddyfile.NewTestDispenser(input)
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile error: %v", err)
	}
	if got := m.Voices["voice-a"]; got != "/tmp/ref-a.wav" {
		t.Errorf("expected top-level voice voice-a -> /tmp/ref-a.wav, got %q", got)
	}
	mc, ok := m.ModelConfigs["model-x"]
	if !ok {
		t.Fatal("expected model-x config")
	}
	if got := mc.Voices["voice-b"]; got != "/tmp/ref-b.wav" {
		t.Errorf("expected model voice voice-b -> /tmp/ref-b.wav, got %q", got)
	}
}

func TestUnmarshalCaddyfileVoiceMissingArg(t *testing.T) {
	input := `reasoning_effort {
		voice voice-a
	}`
	m := ReasoningEffort{}
	d := caddyfile.NewTestDispenser(input)
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("expected an error for a voice directive without a path, got nil")
	}
}

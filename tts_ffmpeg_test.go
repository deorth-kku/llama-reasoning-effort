package reasoningeffort

// Throwaway test: streams /tmp/tts_f32.pcm (ffmpeg-generated float32 LE PCM)
// through pcmResponseWriter in small chunks and writes the int16 result to
// /tmp/tts_s16.pcm for ffmpeg verification. Delete after the manual check.

import (
	"net/http/httptest"
	"os"
	"testing"
)

func TestFFmpegConversionManual(t *testing.T) {
	in, err := os.ReadFile("/tmp/tts_f32.pcm")
	if err != nil {
		t.Skipf("no f32 input file: %v", err)
	}
	outFile, err := os.Create("/tmp/tts_s16.pcm")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	rec := httptest.NewRecorder()
	w := newPcmResponseWriter(rec)
	w.Header().Set("Content-Type", "audio/pcm;rate=24000;encoding=float;bits=32")

	// Stream in awkward chunk sizes to exercise the carry handling.
	for offset := 0; offset < len(in); {
		n := 7
		if rem := len(in) - offset; n > rem {
			n = rem
		}
		if _, err := w.Write(in[offset : offset+n]); err != nil {
			t.Fatal(err)
		}
		offset += n
	}

	if _, err := outFile.Write(rec.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	t.Logf("converted %d f32 bytes -> %d s16 bytes; Content-Type=%q",
		len(in), rec.Body.Len(), rec.Header().Get("Content-Type"))
}

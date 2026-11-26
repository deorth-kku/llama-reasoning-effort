package reasoningeffort

import (
	"bytes"
	"testing"

	"github.com/mactav683/go-nfs-client/rpc"
	"go.uber.org/zap"
)

// credEqual reports whether two OpaqueAuth values carry the same flavor
// and body.
func credEqual(a, b rpc.OpaqueAuth) bool {
	return a.Flavor == b.Flavor && bytes.Equal(a.Body, b.Body)
}

func TestParseNFSURL(t *testing.T) {
	nullCred := rpc.AuthNull()
	sysCred := rpc.AuthSys{UID: 1000, GID: 1000}.OpaqueAuth()
	sysCredGID := rpc.AuthSys{UID: 1000, GID: 1001}.OpaqueAuth()
	tests := []struct {
		raw     string
		want    nfsURL
		wantErr bool
	}{
		{
			raw:  "nfs://192.168.1.201/export/slots",
			want: nfsURL{server: "192.168.1.201:2049", root: "export/slots", cred: nullCred},
		},
		{
			raw:  "nfs://192.168.1.201",
			want: nfsURL{server: "192.168.1.201:2049", root: "", cred: nullCred},
		},
		{
			raw:  "nfs://192.168.1.201/",
			want: nfsURL{server: "192.168.1.201:2049", root: "", cred: nullCred},
		},
		{
			raw:  "nfs://1000@192.168.1.201/export",
			want: nfsURL{server: "192.168.1.201:2049", root: "export", cred: sysCred},
		},
		{
			raw:  "nfs://1000:1001@192.168.1.201:2049/export/slots",
			want: nfsURL{server: "192.168.1.201:2049", root: "export/slots", cred: sysCredGID},
		},
		{
			raw:     "nfs://user@192.168.1.201/export",
			wantErr: true,
		},
		{
			raw:     "nfs://1000:abc@192.168.1.201/export",
			wantErr: true,
		},
		{
			raw:     "http://192.168.1.201/export",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		got, err := parseNFSURL(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseNFSURL(%q): expected error, got %+v", tt.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseNFSURL(%q): %v", tt.raw, err)
			continue
		}
		if got.server != tt.want.server || got.root != tt.want.root || !credEqual(got.cred, tt.want.cred) {
			t.Errorf("parseNFSURL(%q) = server %q root %q cred %+v, want server %q root %q cred %+v",
				tt.raw, got.server, got.root, got.cred, tt.want.server, tt.want.root, tt.want.cred)
		}
	}
}

func TestNewNFSFSFromURLDialFailure(t *testing.T) {
	// Port 1 on localhost refuses connections immediately. The failure is
	// tolerated: the FS is returned disconnected and reconnects on first
	// use.
	fs, err := newNFSFSFromURL("nfs://127.0.0.1:1/export", zap.NewNop())
	if err != nil {
		t.Fatalf("dial failure should be tolerated, got error: %v", err)
	}
	if fs.conn.Load() != nil {
		t.Fatal("expected disconnected FS after dial failure")
	}
}

func TestProvisionSlotLRUNFSInvalidURL(t *testing.T) {
	m := ReasoningEffort{SlotSavePath: "nfs://user@192.168.1.201/export"}
	if err := m.provisionSlotLRU(zap.NewNop()); err == nil {
		t.Fatal("expected error for nfs URL with non-numeric uid, got nil")
	}
}

func TestRedactNFSURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"nfs://1000:1001@192.168.1.201/export/slots", "nfs://***@192.168.1.201/export/slots"},
		{"nfs://192.168.1.201/export/slots", "nfs://192.168.1.201/export/slots"},
		{"/var/lib/llama/slots", "/var/lib/llama/slots"},
	}
	for _, tt := range tests {
		if got := redactNFSURL(tt.in); got != tt.want {
			t.Errorf("redactNFSURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNFSFSPathJoining(t *testing.T) {
	if got := (&nfsFS{cfg: &nfsURL{}}).path("a.bin"); got != "a.bin" {
		t.Errorf("pseudo-root: got %q, want %q", got, "a.bin")
	}
	if got := (&nfsFS{cfg: &nfsURL{root: "export/slots"}}).path("a.bin"); got != "export/slots/a.bin" {
		t.Errorf("subdirectory: got %q, want %q", got, "export/slots/a.bin")
	}
}

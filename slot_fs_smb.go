package reasoningeffort

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// defaultSMBPort is the default SMB server port, used when the URL has no
// port.
const defaultSMBPort = "445"

// smbDialTimeout bounds the TCP dial when connecting to the SMB server in
// Provision.
const smbDialTimeout = 10 * time.Second

// smbOpTimeout bounds each SMB file operation. Slot files and the LRU
// state file are small, so an unreachable or hung server must not stall
// the middleware (or the async LRU goroutines) indefinitely.
const smbOpTimeout = 30 * time.Second

// smbURL is the parsed form of an smb:// slot_save_path URL:
//
//	smb://[[[domain;]username[:password]@]server[:port]/[share/[path/file]]]
type smbURL struct {
	domain   string
	user     string
	password string
	server   string // host:port, port defaulted
	share    string
	root     string // path within the share, "" = share root
}

// parseSMBURL parses an smb:// URL of the form
// smb://[[[domain;]username[:password]@]server[:port]/[share/[path/file]]].
// The first path segment is the share name, the remainder the directory
// within the share. The port defaults to 445.
func parseSMBURL(raw string) (*smbURL, error) {
	// The raw URL may carry credentials, so error messages report only
	// the redacted form (url.Parse errors embed the input verbatim).
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid smb URL %q", redactSMBURL(raw))
	}
	if u.Scheme != "smb" || u.Host == "" {
		return nil, fmt.Errorf("invalid smb URL %q: expected smb://[[[domain;]user[:password]@]server[:port]/share[/path]]", redactSMBURL(raw))
	}
	out := &smbURL{}
	if u.User != nil {
		user := u.User.Username()
		if i := strings.IndexByte(user, ';'); i >= 0 {
			out.domain = user[:i]
			user = user[i+1:]
		}
		out.user = user
		out.password, _ = u.User.Password()
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, defaultSMBPort)
	}
	out.server = host
	rest := strings.Trim(u.Path, "/")
	if rest == "" {
		return nil, fmt.Errorf("invalid smb URL %q: share name is required", redactSMBURL(raw))
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		out.share = rest[:i]
		out.root = rest[i+1:]
	} else {
		out.share = rest
	}
	if out.share == "" {
		return nil, fmt.Errorf("invalid smb URL %q: share name is required", redactSMBURL(raw))
	}
	return out, nil
}

// redactSMBURL masks the userinfo (credentials) of an smb:// URL for
// logging. Strings that are not smb:// URLs are returned unchanged.
func redactSMBURL(raw string) string {
	const scheme = "smb://"
	if !strings.HasPrefix(raw, scheme) {
		return raw
	}
	rest := raw[len(scheme):]
	// userinfo cannot contain '@', so the last '@' separates it from the
	// server.
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		return scheme + "***@" + rest[i+1:]
	}
	return raw
}

// smbFS is the SlotFS implementation backed by an SMB share, accessed
// through the pure-Go go-smb2 client. It lets Caddy manage slot files on a
// share it does not (and need not) mount locally, e.g. when Caddy runs on
// a different host than llama-server.
type smbFS struct {
	session *smb2.Session
	share   *smb2.Share
	root    string // directory within the share; "" = share root
}

// newSMBFSFromURL dials the SMB server from the given smb:// URL, mounts
// the share, and returns a SlotFS rooted at the URL's path within the
// share. Connection failures (bad address, credentials, or share name)
// are returned as errors. A missing root directory is tolerated,
// mirroring the local implementation: the directory may be created later
// (e.g. by llama-server on the host that mounts the share).
func newSMBFSFromURL(raw string) (*smbFS, error) {
	cfg, err := parseSMBURL(raw)
	if err != nil {
		return nil, err
	}
	if cfg.user == "" {
		return nil, fmt.Errorf("smb URL %q: username is required (go-smb2 does not support anonymous access)", redactSMBURL(raw))
	}
	conn, err := net.DialTimeout("tcp", cfg.server, smbDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.server, err)
	}
	dialer := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     cfg.user,
			Password: cfg.password,
			Domain:   cfg.domain,
		},
	}
	session, err := dialer.Dial(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("session setup with %s: %w", cfg.server, err)
	}
	share, err := session.Mount(cfg.share)
	if err != nil {
		_ = session.Logoff()
		return nil, fmt.Errorf("mount share %q: %w", cfg.share, err)
	}
	return &smbFS{session: session, share: share, root: cfg.root}, nil
}

// path joins the share-root directory with the file name. go-smb2
// normalizes "/" separators, so plain forward slashes are fine.
func (f *smbFS) path(name string) string {
	if f.root == "" {
		return name
	}
	return f.root + "/" + name
}

// op returns the share bound to a context that expires after
// smbOpTimeout, bounding a single SMB operation. The caller must invoke
// the returned cancel function (typically via defer) once the operation
// completes.
func (f *smbFS) op() (*smb2.Share, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), smbOpTimeout)
	return f.share.WithContext(ctx), cancel
}

func (f *smbFS) Remove(name string) error {
	s, cancel := f.op()
	defer cancel()
	return s.Remove(f.path(name))
}

func (f *smbFS) Stat(name string) (bool, error) {
	s, cancel := f.op()
	defer cancel()
	_, err := s.Stat(f.path(name))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (f *smbFS) ReadFile(name string) ([]byte, error) {
	s, cancel := f.op()
	defer cancel()
	return s.ReadFile(f.path(name))
}

func (f *smbFS) WriteFile(name string, data []byte) error {
	s, cancel := f.op()
	defer cancel()
	return s.WriteFile(f.path(name), data, 0o666)
}

func (f *smbFS) Rename(oldname, newname string) error {
	// Server-side rename (ReplaceIfExists); atomicity on the share is
	// best-effort.
	s, cancel := f.op()
	defer cancel()
	return s.Rename(f.path(oldname), f.path(newname))
}

func (f *smbFS) ListFiles() ([]SlotFileInfo, error) {
	dir := f.root
	if dir == "" {
		dir = "."
	}
	s, cancel := f.op()
	defer cancel()
	infos, err := s.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]SlotFileInfo, 0, len(infos))
	for _, info := range infos {
		if info.IsDir() {
			continue
		}
		files = append(files, SlotFileInfo{Name: info.Name(), ModTime: info.ModTime()})
	}
	return files, nil
}

// Close disconnects the share and the session, releasing the TCP
// connection. It is called from the module's Cleanup.
func (f *smbFS) Close() error {
	err := f.share.Umount()
	if lerr := f.session.Logoff(); err == nil {
		err = lerr
	}
	return err
}

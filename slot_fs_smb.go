package reasoningeffort

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hirochachacha/go-smb2"
	"go.uber.org/zap"
)

// defaultSMBPort is the default SMB server port, used when the URL has no
// port.
const defaultSMBPort = "445"

// smbConnTimeout bounds the entire SMB connection setup (TCP dial,
// session negotiation, share mount) so a stalled server cannot hang
// startup or a file operation.
const smbConnTimeout = 15 * time.Second

// smbCloseTimeout bounds the disconnect requests (Umount, Logoff) sent
// when tearing down a connection. The go-smb2 client runs them on the
// share's and session's own contexts, which are background; a
// half-open connection (the peer stopped responding without closing the
// TCP connection) would otherwise block them indefinitely, hanging
// Caddy shutdown or a reconnect.
const smbCloseTimeout = 5 * time.Second

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

// smbConn pairs an SMB session with the share mounted on it. The pair is
// swapped in as a single atomic unit when (re)connecting, so readers never
// observe a session and a share from different connections.
type smbConn struct {
	session *smb2.Session
	share   *smb2.Share
}

// smbFS is the SlotFS implementation backed by an SMB share, accessed
// through the pure-Go go-smb2 client. It lets Caddy manage slot files on a
// share it does not (and need not) mount locally, e.g. when Caddy runs on
// a different host than llama-server.
//
// The live connection is held in an atomic pointer and (re)established on
// demand: an operation that fails because the connection is broken
// reconnects once and retries. No locks are used; the atomic pointer is
// the only synchronization.
type smbFS struct {
	cfg  *smbURL
	conn atomic.Pointer[smbConn]
}

// newSMBFSFromURL parses the smb:// URL and attempts to connect, bounded
// in total by smbConnTimeout. A failed connection is not an error: the
// returned SlotFS is left disconnected and reconnects on first use, so a
// down SMB server does not block Caddy startup. Malformed URLs and missing
// credentials are configuration errors and are returned immediately. A
// missing root directory is tolerated, mirroring the local implementation:
// the directory may be created later (e.g. by llama-server on the host that
// mounts the share).
func newSMBFSFromURL(raw string, log *zap.Logger) (*smbFS, error) {
	if log == nil {
		log = zap.NewNop()
	}
	cfg, err := parseSMBURL(raw)
	if err != nil {
		return nil, err
	}
	if cfg.user == "" {
		return nil, fmt.Errorf("smb URL %q: username is required (go-smb2 does not support anonymous access)", redactSMBURL(raw))
	}
	f := &smbFS{cfg: cfg}
	if err := f.connect(); err != nil {
		// Leave the FS disconnected; the first operation reconnects.
		log.Warn("slot SMB: initial connection failed, will retry on first use",
			zap.String("server", cfg.server), zap.Error(err))
	}
	return f, nil
}

// connect dials the SMB server, negotiates a session, and mounts the
// share, bounded in total by smbConnTimeout. The new connection is swapped
// in atomically; the previous one (if any) is disconnected.
func (f *smbFS) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), smbConnTimeout)
	defer cancel()
	conn, err := net.DialTimeout("tcp", f.cfg.server, smbConnTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", f.cfg.server, err)
	}
	dialer := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     f.cfg.user,
			Password: f.cfg.password,
			Domain:   f.cfg.domain,
		},
	}
	session, err := dialer.DialContext(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("session setup with %s: %w", f.cfg.server, err)
	}
	// Mount uses the session's context, which is background; bind the
	// setup timeout to it explicitly.
	share, err := session.WithContext(ctx).Mount(f.cfg.share)
	if err != nil {
		_ = session.Logoff()
		return fmt.Errorf("mount share %q: %w", f.cfg.share, err)
	}
	old := f.conn.Swap(&smbConn{session: session, share: share})
	if old != nil {
		old.close()
	}
	return nil
}

// close disconnects the share and the session, releasing the TCP
// connection. The disconnect requests are bounded in total by
// smbCloseTimeout (see its comment).
func (c *smbConn) close() {
	ctx, cancel := context.WithTimeout(context.Background(), smbCloseTimeout)
	defer cancel()
	_ = c.share.WithContext(ctx).Umount()
	_ = c.session.WithContext(ctx).Logoff()
}

// isConnErr reports whether err indicates a broken SMB connection rather
// than a protocol- or file-level error, i.e. a failure worth reconnecting
// and retrying the operation once. go-smb2 surfaces TCP failures as
// *smb2.TransportError and expired operation deadlines as
// *smb2.ContextError.
func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	var te *smb2.TransportError
	if errors.As(err, &te) {
		return true
	}
	var ce *smb2.ContextError
	if errors.As(err, &ce) {
		return true
	}
	return false
}

// withShare runs op against the current share, reconnecting once and
// retrying if the operation fails because the connection is broken. If no
// connection is established yet, it connects first. op receives a share
// bound to a context that expires after smbOpTimeout, bounding a single
// SMB operation.
func (f *smbFS) withShare(op func(s *smb2.Share) error) error {
	c := f.conn.Load()
	if c == nil {
		if err := f.connect(); err != nil {
			return err
		}
		if c = f.conn.Load(); c == nil {
			// Closed between the connect and the load.
			return fmt.Errorf("smb connection closed")
		}
	}
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), smbOpTimeout)
		err := op(c.share.WithContext(ctx))
		cancel()
		if err == nil || attempt > 0 || !isConnErr(err) {
			return err
		}
		// The connection is broken.
		cur := f.conn.Load()
		if cur == nil {
			// The FS was closed; do not reconnect.
			return err
		}
		if cur != c {
			// Another goroutine already reconnected; reuse its connection.
			c = cur
			continue
		}
		if rerr := f.connect(); rerr != nil {
			return fmt.Errorf("smb reconnect: %w", rerr)
		}
		if c = f.conn.Load(); c == nil {
			return fmt.Errorf("smb connection closed")
		}
	}
}

// path joins the share-root directory with the file name. go-smb2
// normalizes "/" separators, so plain forward slashes are fine.
func (f *smbFS) path(name string) string {
	if f.cfg.root == "" {
		return name
	}
	return f.cfg.root + "/" + name
}

func (f *smbFS) Remove(name string) error {
	return f.withShare(func(s *smb2.Share) error {
		return s.Remove(f.path(name))
	})
}

func (f *smbFS) Stat(name string) (bool, error) {
	exists := false
	err := f.withShare(func(s *smb2.Share) error {
		_, e := s.Stat(f.path(name))
		if e == nil {
			exists = true
			return nil
		}
		if os.IsNotExist(e) {
			return nil
		}
		return e
	})
	return exists, err
}

func (f *smbFS) ReadFile(name string) ([]byte, error) {
	var data []byte
	if err := f.withShare(func(s *smb2.Share) error {
		var e error
		data, e = s.ReadFile(f.path(name))
		return e
	}); err != nil {
		return nil, err
	}
	return data, nil
}

func (f *smbFS) WriteFile(name string, data []byte) error {
	return f.withShare(func(s *smb2.Share) error {
		return s.WriteFile(f.path(name), data, 0o666)
	})
}

func (f *smbFS) Rename(oldname, newname string) error {
	// Server-side rename (ReplaceIfExists); atomicity on the share is
	// best-effort.
	return f.withShare(func(s *smb2.Share) error {
		return s.Rename(f.path(oldname), f.path(newname))
	})
}

func (f *smbFS) ListFiles() ([]SlotFileInfo, error) {
	dir := f.cfg.root
	if dir == "" {
		dir = "."
	}
	var files []SlotFileInfo
	if err := f.withShare(func(s *smb2.Share) error {
		infos, e := s.ReadDir(dir)
		if e != nil {
			return e
		}
		files = make([]SlotFileInfo, 0, len(infos))
		for _, info := range infos {
			if info.IsDir() {
				continue
			}
			files = append(files, SlotFileInfo{Name: info.Name(), ModTime: info.ModTime()})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return files, nil
}

// Close disconnects the current share and session, releasing the TCP
// connection. It is called from the module's Cleanup. Swapping the
// connection pointer to nil prevents a concurrent operation from
// resurrecting the connection after shutdown.
func (f *smbFS) Close() error {
	c := f.conn.Swap(nil)
	if c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), smbCloseTimeout)
	defer cancel()
	err := c.share.WithContext(ctx).Umount()
	if lerr := c.session.WithContext(ctx).Logoff(); err == nil {
		err = lerr
	}
	return err
}

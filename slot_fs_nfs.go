package reasoningeffort

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	nfsclient "github.com/mactav683/go-nfs-client/client"
	"github.com/mactav683/go-nfs-client/nfs4"
	"github.com/mactav683/go-nfs-client/rpc"
	"go.uber.org/zap"
)

// defaultNFSPort is the default NFS server port, used when the URL has no
// port. NFSv4 always runs on a single well-known port; no portmapper
// lookup is involved.
const defaultNFSPort = "2049"

// nfsConnTimeout bounds the entire NFS connection setup (TCP dial,
// SETCLIENTID, root filehandle fetch, export directory lookup) so a
// stalled server cannot hang startup or a file operation.
const nfsConnTimeout = 15 * time.Second

// nfsOpTimeout bounds each NFS file operation. Slot files and the LRU
// state file are small, so an unreachable or hung server must not stall
// the middleware (or the async LRU goroutines) indefinitely. An operation
// that hits the deadline also kills the connection (the client library
// closes it when an operation's context expires), so the next operation
// reconnects.
const nfsOpTimeout = 30 * time.Second

// nfsLeasePeriod is the NFSv4 lease period assumed for RENEW scheduling.
// The RFC 7530 default lease time is 60s; the library's lease manager
// renews at two-thirds of this period.
const nfsLeasePeriod = 60 * time.Second

// nfsSourcePort is the local (client-side) TCP port we bind when dialing
// the NFS server. It must be < 1024 so the server's default `secure` export
// option accepts our connection: nfsd checks the client's source port and
// returns NFS4ERR_PERM on the export point for non-privileged ports. The
// kernel NFS client satisfies this implicitly by running as root, but our
// pure-Go client would otherwise use an ephemeral port (>1024). Caddy holds
// CAP_NET_BIND_SERVICE (it binds 443), so binding a low port here succeeds.
const nfsSourcePort = 1000

// nfsURL is the parsed form of an nfs:// slot_save_path URL:
//
//	nfs://[[uid[:gid]@]host[:port]/[export/path/dir]]
//
// The path is the directory that holds the slot files, relative to the
// server's pseudo-root. The optional userinfo carries the AUTH_SYS
// credential (numeric uid, and optionally gid); when absent, AUTH_NULL is
// used.
type nfsURL struct {
	server string // host:port, port defaulted
	root   string // directory under the pseudo-root, "" = pseudo-root
	cred   rpc.OpaqueAuth
}

// parseNFSURL parses an nfs:// URL of the form
// nfs://[[uid[:gid]@]host[:port]/[export/path/dir]]. The port defaults to
// 2049. The userinfo, when present, must be a numeric uid (optionally
// followed by :gid); gid defaults to uid.
func parseNFSURL(raw string) (*nfsURL, error) {
	// The raw URL may carry credentials, so error messages report only
	// the redacted form (url.Parse errors embed the input verbatim).
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid nfs URL %q", redactNFSURL(raw))
	}
	if u.Scheme != "nfs" || u.Host == "" {
		return nil, fmt.Errorf("invalid nfs URL %q: expected nfs://[[uid[:gid]@]host[:port]/[export/path/dir]]", redactNFSURL(raw))
	}
	out := &nfsURL{cred: rpc.AuthNull()}
	if u.User != nil {
		uid, err := strconv.ParseUint(u.User.Username(), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid nfs URL %q: uid must be a number", redactNFSURL(raw))
		}
		gid := uint32(uid)
		if gidStr, ok := u.User.Password(); ok && gidStr != "" {
			g, err := strconv.ParseUint(gidStr, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid nfs URL %q: gid must be a number", redactNFSURL(raw))
			}
			gid = uint32(g)
		}
		out.cred = rpc.AuthSys{UID: uint32(uid), GID: gid}.OpaqueAuth()
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, defaultNFSPort)
	}
	out.server = host
	out.root = strings.Trim(u.Path, "/")
	return out, nil
}

// redactNFSURL masks the userinfo (credential) of an nfs:// URL for
// logging. Strings that are not nfs:// URLs are returned unchanged.
func redactNFSURL(raw string) string {
	const scheme = "nfs://"
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

// nfsConn pairs the mounted NFSv4 connection with the lease manager that
// keeps its clientid and open stateids alive. The pair is swapped in as a
// single atomic unit when (re)connecting, so readers never observe a
// connection and a lease manager from different connections.
type nfsConn struct {
	conn *nfs4.Conn
	lm   *nfsclient.LeaseManager
}

// close stops the RENEW loop and releases the connection.
func (c *nfsConn) close() {
	if c.lm != nil {
		c.lm.Stop()
	}
	_ = c.conn.Close()
}

// nfsFS is the SlotFS implementation backed by an NFSv4 export, accessed
// through the pure-Go go-nfs-client. It lets Caddy manage slot files on an
// export it does not (and need not) mount locally, e.g. when Caddy runs on
// a different host than llama-server.
//
// The live connection is held in an atomic pointer and (re)established on
// demand: an operation that fails because the connection is broken
// reconnects once and retries. No locks are used; the atomic pointer is
// the only synchronization.
type nfsFS struct {
	cfg  *nfsURL
	conn atomic.Pointer[nfsConn]
}

// newNFSFSFromURL parses the nfs:// URL and attempts to connect, bounded
// in total by nfsConnTimeout. A failed connection is not an error: the
// returned SlotFS is left disconnected and reconnects on first use, so a
// down NFS server does not block Caddy startup. Malformed URLs are
// configuration errors and are returned immediately. A missing root
// directory is tolerated, mirroring the local implementation: the
// directory may be created later (e.g. by llama-server on the host that
// mounts the export).
func newNFSFSFromURL(raw string, log *zap.Logger) (*nfsFS, error) {
	if log == nil {
		log = zap.NewNop()
	}
	cfg, err := parseNFSURL(raw)
	if err != nil {
		return nil, err
	}
	f := &nfsFS{cfg: cfg}
	if err := f.connect(); err != nil {
		// Leave the FS disconnected; the first operation reconnects.
		log.Warn("slot NFS: initial connection failed, will retry on first use",
			zap.String("server", cfg.server), zap.Error(err))
	}
	return f, nil
}

// connect dials the NFS server from a privileged source port, establishes
// the clientid, and resolves the root directory, bounded in total by
// nfsConnTimeout. The new connection is swapped in atomically; the previous
// one (if any) is closed.
func (f *nfsFS) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), nfsConnTimeout)
	defer cancel()
	conn, err := f.dialNFS(ctx)
	if err != nil {
		return fmt.Errorf("mount %s%s: %w", f.cfg.server, f.cfg.root, err)
	}
	lm := nfsclient.NewLeaseManager(nfsclient.LeaseConfig{
		LeasePeriod: nfsLeasePeriod,
		Renew:       func(ctx context.Context) error { return conn.Renew(ctx) },
		Recover:     func(ctx context.Context) error { return conn.Mount(ctx) },
	})
	lm.Start()
	old := f.conn.Swap(&nfsConn{conn: conn, lm: lm})
	if old != nil {
		// A concurrent operation may still be using old. Closing it here
		// fails that operation with a connection error, which withClient
		// classifies as worth one retry on the new connection.
		old.close()
	}
	return nil
}

// dialNFS dials the server from a privileged source port and mounts an
// NFSv4 connection, negotiating the highest supported minor version (v4.1,
// falling back to v4.0). The returned connection is open and owned by the
// caller.
func (f *nfsFS) dialNFS(ctx context.Context) (*nfs4.Conn, error) {
	for _, minor := range []uint32{nfs4.MinorV41, nfs4.MinorV40} {
		conn, err := f.dialNFSVersion(ctx, minor)
		if err == nil {
			return conn, nil
		}
		if !isNFSMinorVersionFallback(err) {
			return nil, err
		}
	}
	// Unreachable: the loop returns on success or a non-fallback error.
	return nil, fmt.Errorf("dial %s: minor version negotiation failed", f.cfg.server)
}

// dialNFSVersion dials the server from the privileged source port and
// mounts with the given NFSv4 minor version. On failure the TCP connection
// is closed and the error returned.
func (f *nfsFS) dialNFSVersion(ctx context.Context, minor uint32) (*nfs4.Conn, error) {
	local := &net.TCPAddr{Port: nfsSourcePort}
	d := net.Dialer{LocalAddr: local}
	tcpConn, err := d.DialContext(ctx, "tcp", f.cfg.server)
	if err != nil {
		return nil, fmt.Errorf("dial %s from local port %d: %w", f.cfg.server, nfsSourcePort, err)
	}
	if tc, ok := tcpConn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	rpcClient := rpc.NewClient(tcpConn, rpc.ClientOptions{Cred: f.cfg.cred, Verf: rpc.AuthNull()})
	conn := nfs4.NewConn(nfs4.NewRPCCaller(rpcClient), nfs4.ConnConfig{MinorVersion: minor})
	if err := conn.Mount(ctx); err != nil {
		_ = rpcClient.Close()
		return nil, err
	}
	return conn, nil
}

// isNFSMinorVersionFallback reports whether err indicates the server does
// not support the attempted (v4.1) minor version and a v4.0 retry is
// warranted. Mirrors the library's DialAuto fallback set.
func isNFSMinorVersionFallback(err error) bool {
	var se *nfs4.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Status {
	case nfs4.NFS4ERR_MINOR_VERS_MISMATCH, nfs4.NFS4ERR_NOTSUPP, nfs4.NFS4ERR_OP_ILLEGAL:
		return true
	}
	return false
}

// isNFSConnErr reports whether err indicates a broken NFS connection or
// stale client state rather than a protocol- or file-level error, i.e. a
// failure worth reconnecting and retrying the operation once. The client
// library closes the connection when an operation's context expires (a
// cancelled connection is not reusable), so an expired deadline is a
// connection failure. Stale filehandles, expired filehandles, missing
// filehandles and server faults indicate lost server-side state, which a
// reconnect re-establishes.
func isNFSConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var se *nfs4.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case nfs4.NFS4ERR_STALE, nfs4.NFS4ERR_FHEXPIRED, nfs4.NFS4ERR_NOFILEHANDLE, nfs4.NFS4ERR_SERVERFAULT:
			return true
		}
	}
	var oe *net.OpError
	return errors.As(err, &oe)
}

// withClient runs op against the current client, reconnecting once and
// retrying if the operation fails because the connection is broken. If no
// connection is established yet, it connects first. op receives the
// client and a context that expires after nfsOpTimeout, bounding a single
// operation.
func (f *nfsFS) withClient(op func(c *nfs4.Conn, ctx context.Context) error) error {
	c := f.conn.Load()
	if c == nil {
		if err := f.connect(); err != nil {
			return err
		}
		if c = f.conn.Load(); c == nil {
			// Closed between the connect and the load.
			return fmt.Errorf("nfs connection closed")
		}
	}
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), nfsOpTimeout)
		err := op(c.conn, ctx)
		cancel()
		if err == nil || attempt > 0 || !isNFSConnErr(err) {
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
			return fmt.Errorf("nfs reconnect: %w", rerr)
		}
		if c = f.conn.Load(); c == nil {
			return fmt.Errorf("nfs connection closed")
		}
	}
}

// path joins the export-root directory with the file name. NFSv4 paths
// are relative to the server pseudo-root and use plain forward slashes.
func (f *nfsFS) path(name string) string {
	if f.cfg.root == "" {
		return name
	}
	return f.cfg.root + "/" + name
}

// lookupDir resolves dir (a path relative to the pseudo-root, "" = root)
// to its filehandle.
func (f *nfsFS) lookupDir(ctx context.Context, conn *nfs4.Conn, dir string) (nfs4.FileHandle, error) {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return conn.RootFH(), nil
	}
	return conn.LookupPath(ctx, strings.Split(dir, "/"))
}

func (f *nfsFS) Remove(name string) error {
	return f.withClient(func(conn *nfs4.Conn, ctx context.Context) error {
		dir, base := path.Split(f.path(name))
		fh, err := f.lookupDir(ctx, conn, dir)
		if err != nil {
			return err
		}
		return conn.Remove(ctx, fh, base)
	})
}

func (f *nfsFS) Stat(name string) (bool, error) {
	exists := false
	err := f.withClient(func(conn *nfs4.Conn, ctx context.Context) error {
		_, e := nfsclient.New(conn).WithContext(ctx).Stat(f.path(name))
		if e == nil {
			exists = true
			return nil
		}
		if errors.Is(e, fs.ErrNotExist) {
			return nil
		}
		return e
	})
	return exists, err
}

func (f *nfsFS) ReadFile(name string) ([]byte, error) {
	var data []byte
	if err := f.withClient(func(conn *nfs4.Conn, ctx context.Context) error {
		file, e := nfsclient.New(conn).WithContext(ctx).Open(f.path(name))
		if e != nil {
			return e
		}
		defer file.Close()
		data, e = io.ReadAll(file)
		return e
	}); err != nil {
		return nil, err
	}
	return data, nil
}

func (f *nfsFS) WriteFile(name string, data []byte) error {
	return f.withClient(func(conn *nfs4.Conn, ctx context.Context) error {
		// O_WRONLY selects OPEN_SHARE_ACCESS_WRITE; without a direction
		// bit the client library would open with READ share access and
		// silently skip the O_TRUNC truncation.
		file, err := nfsclient.NewRW(conn).WithContext(ctx).OpenFile(ctx, f.path(name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, werr := file.Write(data)
		cerr := file.Close()
		if werr != nil {
			return werr
		}
		return cerr
	})
}

func (f *nfsFS) Rename(oldname, newname string) error {
	// Server-side RENAME; atomicity on the export is best-effort.
	return f.withClient(func(conn *nfs4.Conn, ctx context.Context) error {
		oldDir, oldBase := path.Split(f.path(oldname))
		newDir, newBase := path.Split(f.path(newname))
		srcFH, err := f.lookupDir(ctx, conn, oldDir)
		if err != nil {
			return err
		}
		dstFH, err := f.lookupDir(ctx, conn, newDir)
		if err != nil {
			return err
		}
		return conn.Rename(ctx, srcFH, oldBase, dstFH, newBase)
	})
}

func (f *nfsFS) ListFiles() ([]SlotFileInfo, error) {
	dir := f.cfg.root
	if dir == "" {
		dir = "."
	}
	var files []SlotFileInfo
	if err := f.withClient(func(conn *nfs4.Conn, ctx context.Context) error {
		entries, e := nfsclient.New(conn).WithContext(ctx).ReadDir(dir)
		if e != nil {
			return e
		}
		files = make([]SlotFileInfo, 0, len(entries))
		for _, entry := range entries {
			info, ierr := entry.Info()
			if ierr != nil {
				return ierr
			}
			if !info.Mode().IsRegular() {
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

// Close disconnects the current client, releasing the TCP connection. It
// is called from the module's Cleanup. Swapping the connection pointer to
// nil stops operations that have not started yet from reconnecting
// (withClient treats a nil connection as "closed, do not reconnect"). A
// concurrent operation that already observed a nil connection and is
// inside connect() may still swap in a new connection after Close
// returns, resurrecting it and its RENEW loop; the window is tiny and
// harmless at shutdown.
func (f *nfsFS) Close() error {
	c := f.conn.Swap(nil)
	if c == nil {
		return nil
	}
	c.close()
	return nil
}

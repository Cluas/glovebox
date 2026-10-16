package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

type server struct {
	cfg    *configStore
	auth   *auth
	zte    *zteClient
	mihomo *mihomoClient
	net    *netSampler
	ts     *tsProbe
	certs  *certManager // nil when HTTPS is off
	logDir string
	// demo mode: in-process fake ZTE / mihomo backends, synthetic system info,
	// destructive actions are only logged
	demo bool
	sys  func() sysInfo

	zteMu    sync.Mutex
	zteCache map[string]any
	zteAt    time.Time
	zteErr   error

	sigMu   sync.Mutex
	sigHist []sigSample

	capMu  sync.Mutex
	capMax int
	capAt  time.Time
}

func newServer(cs *configStore, logDir string) *server {
	c := cs.get()
	mc := newMihomo(c.MihomoAPI, c.MihomoSecret)
	return &server{
		cfg:    cs,
		auth:   &auth{cfg: cs},
		zte:    newZTE(c.ZTEBase, c.ZTEDB),
		mihomo: mc,
		net:    &netSampler{wanPrefixes: c.WANPrefixes, lanDev: c.LANDev},
		ts:     &tsProbe{m: mc, cfg: cs},
		logDir: logDir,
		sys:    readSysInfo,
	}
}

// run starts the samplers and the HTTP / HTTPS listeners until ctx is cancelled
func (s *server) run(ctx context.Context) error {
	c := s.cfg.get()
	go s.net.loop(ctx)
	go s.sigLoop(ctx)
	go s.ts.loop(ctx)
	go ensureAlias(ctx, c.AliasAddr, c.AliasDev)

	h := s.routes()
	newSrv := func() *http.Server {
		return &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			ErrorLog:          log.New(quietErrors{}, "http: ", 0),
		}
	}
	// HTTP and HTTPS use separate servers: net/http configures HTTP/2 once per server, and a
	// plain Serve that runs first would leave the TLS listener advertising h2 without a handler.
	srv := newSrv()
	servers := []*http.Server{srv}
	errc := make(chan error, len(c.Listen)+len(c.TLSListen))
	for _, addr := range c.Listen {
		ln, err := listenTCP(addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		log.Printf("glovebox %s listening on http://%s", version, addr)
		go func() { errc <- srv.Serve(ln) }()
	}
	if s.certs != nil && len(c.TLSListen) > 0 {
		tlsSrv := newSrv()
		tlsSrv.TLSConfig = &tls.Config{GetCertificate: s.certs.getCertificate, MinVersion: tls.VersionTLS12}
		servers = append(servers, tlsSrv)
		for _, addr := range c.TLSListen {
			ln, err := listenTCP(addr)
			if err != nil {
				// HTTPS is optional; failing to bind must not take HTTP down
				log.Printf("HTTPS listen %s: %v", addr, err)
				continue
			}
			log.Printf("glovebox %s listening on https://%s", version, addr)
			go func() { errc <- tlsSrv.ServeTLS(ln, "", "") }()
		}
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Printf("HTTP server stopped: %v", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, sv := range servers {
		sv.Shutdown(sctx)
	}
	return nil
}

// quietErrors forwards net/http server errors to the log, except TLS handshake failures, which
// every browser that has not trusted the local CA yet produces on each visit
type quietErrors struct{}

func (quietErrors) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("TLS handshake error")) {
		log.Print(string(p))
	}
	return len(p), nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /ca.crt", s.handleCA)

	authed := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !s.auth.valid(r) {
				writeErr(w, http.StatusUnauthorized, apiErr("unauthorized", "please log in"))
				return
			}
			h(w, r)
		})
	}
	authed("GET /api/info", s.handleInfo)
	authed("GET /api/status", s.handleStatus)
	authed("GET /api/proxies", s.handleProxies)
	authed("POST /api/proxies/select", s.handleSelect)
	authed("POST /api/proxies/delay", s.handleDelay)
	authed("POST /api/proxies/mode", s.handleMode)
	authed("POST /api/proxies/update", s.handleUpdateProviders)
	authed("POST /api/proxies/close-connections", s.handleCloseConns)
	authed("GET /api/connections", s.handleConnections)
	authed("POST /api/tailscale/check", s.handleTSCheck)
	authed("GET /api/clients", s.handleClients)
	authed("GET /api/wifi", s.handleWiFi)
	authed("POST /api/wifi", s.handleSetWiFi)
	authed("POST /api/box/restart", s.handleBoxRestart)
	authed("POST /api/device/reboot", s.handleReboot)
	authed("POST /api/device/time", s.handleSetTime)
	authed("POST /api/console/password", s.handlePassword)
	authed("GET /api/logs", s.handleLogs)

	mux.Handle("GET /", staticHandler())
	return s.guard(mux)
}

const csp = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; " +
	"connect-src 'self'; worker-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// guard only lets the hotspot subnet and localhost in. Non-GET requests must carry the
// X-Glovebox header, which cross-site pages cannot set without a CORS preflight (CSRF protection).
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedRemote(r.RemoteAddr, s.cfg.get().AllowCIDRs) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Glovebox") != "1" {
			http.Error(w, "missing X-Glovebox header", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func allowedRemote(remote string, cidrs []string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(ip) {
			return true
		}
	}
	return false
}

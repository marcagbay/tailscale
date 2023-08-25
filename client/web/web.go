// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package web provides the Tailscale client for web.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gorilla/csrf"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/licenses"
	"tailscale.com/net/netutil"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
	"tailscale.com/util/httpm"
	"tailscale.com/version/distro"
)

// This contains all files needed to build the frontend assets.
// Because we assign this to the blank identifier, it does not actually embed the files.
// However, this does cause `go mod vendor` to include the files when vendoring the package.
// External packages that use the web client can `go mod vendor`, run `yarn build` to
// build the assets, then those asset bundles will be able to be embedded.
//
//go:embed yarn.lock index.html *.js *.json src/*
var _ embed.FS

//go:embed web.html web.css
var embeddedFS embed.FS

var tmpls *template.Template

// Server is the backend server for a Tailscale web client.
type Server struct {
	lc *tailscale.LocalClient

	devMode  bool
	devProxy *httputil.ReverseProxy // only filled when devMode is on

	cgiMode    bool
	cgiPath    string
	apiHandler http.Handler // csrf-protected api handler

	selfMu sync.Mutex // protects self field
	// self is a cached NodeView of the active self node,
	// refreshed by watching the IPN notification bus
	// (see Server.watchSelf).
	//
	// self's hostname and Tailscale IP are used to verify
	// that incoming requests to the web client api are coming
	// from the web client frontend and not some other source.
	// Particularly to protect against DNS rebinding attacks.
	// self should not be used to fill data for frontend views.
	self tailcfg.NodeView
}

// ServerOpts contains options for constructing a new Server.
type ServerOpts struct {
	DevMode bool

	// CGIMode indicates if the server is running as a CGI script.
	CGIMode bool

	// If running in CGIMode, CGIPath is the URL path prefix to the CGI script.
	CGIPath string

	// LocalClient is the tailscale.LocalClient to use for this web server.
	// If nil, a new one will be created.
	LocalClient *tailscale.LocalClient
}

// NewServer constructs a new Tailscale web client server.
// The provided context should live for the duration of the Server's lifetime.
func NewServer(ctx context.Context, opts ServerOpts) (s *Server, cleanup func()) {
	if opts.LocalClient == nil {
		opts.LocalClient = &tailscale.LocalClient{}
	}
	s = &Server{
		devMode: opts.DevMode,
		lc:      opts.LocalClient,
		cgiMode: opts.CGIMode,
		cgiPath: opts.CGIPath,
	}
	cleanup = func() {}
	if s.devMode {
		cleanup = s.startDevServer()
		s.addProxyToDevServer()

		// Create handler for "/api" requests with CSRF protection.
		// We don't require secure cookies, since the web client is regularly used
		// on network appliances that are served on local non-https URLs.
		// The client is secured by limiting the interface it listens on,
		// or by authenticating requests before they reach the web client.
		csrfProtect := csrf.Protect(s.csrfKey(), csrf.Secure(false))
		s.apiHandler = csrfProtect(http.HandlerFunc(s.serveAPI))
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		go s.watchSelf(ctx)
	}()

	s.lc.IncrementCounter(context.Background(), "web_client_initialization", 1)
	return s, cleanup
}

func init() {
	tmpls = template.Must(template.New("").ParseFS(embeddedFS, "*"))
}

// watchSelf watches the IPN notification bus to refresh
// the Server's self node cache.
func (s *Server) watchSelf(ctx context.Context) {
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	watcher, err := s.lc.WatchIPNBus(watchCtx, ipn.NotifyInitialNetMap|ipn.NotifyNoPrivateKeys)
	if err != nil {
		log.Fatalf("lost connection to tailscaled: %v", err)
	}
	defer watcher.Close()

	for {
		n, err := watcher.Next()
		if err != nil {
			log.Fatalf("lost connection to tailscaled: %v", err)
		}
		if state := n.State; state != nil && *state == ipn.NeedsLogin {
			s.updateSelf(tailcfg.NodeView{})
			continue
		}
		if n.NetMap == nil {
			continue
		}
		s.updateSelf(n.NetMap.SelfNode)
	}
}

// updateSelf grabs the lock and updates s.self.
// Then logs if anything changed.
func (s *Server) updateSelf(self tailcfg.NodeView) {
	s.selfMu.Lock()
	prev := s.self
	s.self = self
	s.selfMu.Unlock()

	var old, new tailcfg.StableNodeID
	if prev.Valid() {
		old = prev.StableID()
	}
	if s.self.Valid() {
		new = s.self.StableID()
	}
	if old != new {
		if new.IsZero() {
			log.Printf("self node logout")
		} else {
			log.Printf("self node login")
		}
	}
}

// ServeHTTP processes all requests for the Tailscale web client.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// some platforms where the client runs have their own authentication
	// and authorization mechanisms we need to work with. Do those checks first.
	switch distro.Get() {
	case distro.Synology:
		if authorizeSynology(w, r) {
			return
		}
	case distro.QNAP:
		if authorizeQNAP(w, r) {
			return
		}
	}

	handler := s.serve

	// if running in cgi mode, strip the cgi path prefix
	if s.cgiMode {
		prefix := s.cgiPath
		if prefix == "" {
			switch distro.Get() {
			case distro.Synology:
				prefix = synologyPrefix
			case distro.QNAP:
				prefix = qnapPrefix
			}
		}
		if prefix != "" {
			handler = enforcePrefix(prefix, handler)
		}
	}

	handler(w, r)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if s.devMode {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Pass through to other handlers via CSRF protection.
			s.apiHandler.ServeHTTP(w, r)
			return
		}
		// When in dev mode, proxy to the Vite dev server.
		s.devProxy.ServeHTTP(w, r)
		return
	}

	switch {
	case r.Method == "POST":
		s.servePostNodeUpdate(w, r)
		return
	default:
		s.lc.IncrementCounter(context.Background(), "web_client_page_load", 1)
		s.serveGetNodeData(w, r)
		return
	}
}

// serveAPI serves requests for the web client api.
// It should only be called by Server.ServeHTTP, via Server.apiHandler,
// which protects the handler using gorilla csrf.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-CSRF-Token", csrf.Token(r))
	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case path == "/data":
		switch r.Method {
		case httpm.GET:
			s.serveGetNodeDataJSON(w, r)
		case httpm.POST:
			s.servePostNodeUpdate(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	case strings.HasPrefix(path, "/local/"):
		s.proxyRequestToLocalAPI(w, r)
		return
	}
	http.Error(w, "invalid endpoint", http.StatusNotFound)
}

type nodeData struct {
	Profile           tailcfg.UserProfile
	Status            string
	DeviceName        string
	IP                string
	AdvertiseExitNode bool
	AdvertiseRoutes   string
	LicensesURL       string
	TUNMode           bool
	IsSynology        bool
	DSMVersion        int // 6 or 7, if IsSynology=true
	IsUnraid          bool
	UnraidToken       string
	IPNVersion        string
}

func (s *Server) getNodeData(ctx context.Context) (*nodeData, error) {
	st, err := s.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	prefs, err := s.lc.GetPrefs(ctx)
	if err != nil {
		return nil, err
	}
	profile := st.User[st.Self.UserID]
	deviceName := strings.Split(st.Self.DNSName, ".")[0]
	versionShort := strings.Split(st.Version, "-")[0]
	data := &nodeData{
		Profile:     profile,
		Status:      st.BackendState,
		DeviceName:  deviceName,
		LicensesURL: licenses.LicensesURL(),
		TUNMode:     st.TUN,
		IsSynology:  distro.Get() == distro.Synology || envknob.Bool("TS_FAKE_SYNOLOGY"),
		DSMVersion:  distro.DSMVersion(),
		IsUnraid:    distro.Get() == distro.Unraid,
		UnraidToken: os.Getenv("UNRAID_CSRF_TOKEN"),
		IPNVersion:  versionShort,
	}
	exitNodeRouteV4 := netip.MustParsePrefix("0.0.0.0/0")
	exitNodeRouteV6 := netip.MustParsePrefix("::/0")
	for _, r := range prefs.AdvertiseRoutes {
		if r == exitNodeRouteV4 || r == exitNodeRouteV6 {
			data.AdvertiseExitNode = true
		} else {
			if data.AdvertiseRoutes != "" {
				data.AdvertiseRoutes += ","
			}
			data.AdvertiseRoutes += r.String()
		}
	}
	if len(st.TailscaleIPs) != 0 {
		data.IP = st.TailscaleIPs[0].String()
	}
	return data, nil
}

func (s *Server) serveGetNodeData(w http.ResponseWriter, r *http.Request) {
	data, err := s.getNodeData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf := new(bytes.Buffer)
	if err := tmpls.ExecuteTemplate(buf, "web.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(buf.Bytes())
}

func (s *Server) serveGetNodeDataJSON(w http.ResponseWriter, r *http.Request) {
	data, err := s.getNodeData(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(*data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	return
}

type nodeUpdate struct {
	AdvertiseRoutes   string
	AdvertiseExitNode bool
	Reauthenticate    bool
	ForceLogout       bool
}

func (s *Server) servePostNodeUpdate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	st, err := s.lc.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var postData nodeUpdate
	type mi map[string]any
	if err := json.NewDecoder(r.Body).Decode(&postData); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(mi{"error": err.Error()})
		return
	}

	routes, err := netutil.CalcAdvertiseRoutes(postData.AdvertiseRoutes, postData.AdvertiseExitNode)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(mi{"error": err.Error()})
		return
	}
	mp := &ipn.MaskedPrefs{
		AdvertiseRoutesSet: true,
		WantRunningSet:     true,
	}
	mp.Prefs.WantRunning = true
	mp.Prefs.AdvertiseRoutes = routes
	log.Printf("Doing edit: %v", mp.Pretty())

	if _, err := s.lc.EditPrefs(r.Context(), mp); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(mi{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var reauth, logout bool
	if postData.Reauthenticate {
		reauth = true
	}
	if postData.ForceLogout {
		logout = true
	}
	log.Printf("tailscaleUp(reauth=%v, logout=%v) ...", reauth, logout)
	url, err := s.tailscaleUp(r.Context(), st, postData)
	log.Printf("tailscaleUp = (URL %v, %v)", url != "", err)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(mi{"error": err.Error()})
		return
	}
	if url != "" {
		json.NewEncoder(w).Encode(mi{"url": url})
	} else {
		io.WriteString(w, "{}")
	}
	return
}

func (s *Server) tailscaleUp(ctx context.Context, st *ipnstate.Status, postData nodeUpdate) (authURL string, retErr error) {
	if postData.ForceLogout {
		if err := s.lc.Logout(ctx); err != nil {
			return "", fmt.Errorf("Logout error: %w", err)
		}
		return "", nil
	}

	origAuthURL := st.AuthURL
	isRunning := st.BackendState == ipn.Running.String()

	forceReauth := postData.Reauthenticate
	if !forceReauth {
		if origAuthURL != "" {
			return origAuthURL, nil
		}
		if isRunning {
			return "", nil
		}
	}

	// printAuthURL reports whether we should print out the
	// provided auth URL from an IPN notify.
	printAuthURL := func(url string) bool {
		return url != origAuthURL
	}

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watcher, err := s.lc.WatchIPNBus(watchCtx, 0)
	if err != nil {
		return "", err
	}
	defer watcher.Close()

	go func() {
		if !isRunning {
			s.lc.Start(ctx, ipn.Options{})
		}
		if forceReauth {
			s.lc.StartLoginInteractive(ctx)
		}
	}()

	for {
		n, err := watcher.Next()
		if err != nil {
			return "", err
		}
		if n.ErrMessage != nil {
			msg := *n.ErrMessage
			return "", fmt.Errorf("backend error: %v", msg)
		}
		if url := n.BrowseToURL; url != nil && printAuthURL(*url) {
			return *url, nil
		}
	}
}

// proxyRequestToLocalAPI proxies the web API request to the localapi.
//
// The web API request path is expected to exactly match a localapi path,
// with prefix /api/local/ rather than /localapi/.
//
// If the request comes from a host other than the node itself,
// the request is rejected.
func (s *Server) proxyRequestToLocalAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/local/")
	if r.URL.Path == path { // missing prefix
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// First, we verify that the request came from the node's web client
	// frontend, and not from some other source. We do this by checking
	// that the request host matches the known names for the self node.

	s.selfMu.Lock()
	self := s.self
	s.selfMu.Unlock()
	if !self.Valid() {
		// If there's no node logged in, reject all localapi proxy requests.
		//
		// Login requests are handled outside of the localapi proxy from the
		// Server.tailscaleUp handler, so we should always have a valid node
		// when using the localapi proxy.
		http.Error(w, "no authenticated node", http.StatusForbidden)
		return
	}

	dnsSuffix := netmap.MagicDNSSuffixOfNodeName(self.Name())
	nodeFQDN := strings.TrimSuffix(self.Name(), ".")           // node.tailnet.ts.net
	nodeName := strings.TrimSuffix(self.Name(), "."+dnsSuffix) // without tailnet suffix

	requestHost, _, _ := net.SplitHostPort(r.Host)
	switch requestHost {
	case nodeFQDN, nodeName, self.Hostinfo().Hostname(), "localhost", "127.0.0.1":
		// TODO: check self.addresses as well (incase they access using IP)
		break // request host is a known name for this node
	default:
		http.Error(w, fmt.Sprintf("request from unknown host %q", requestHost), http.StatusForbidden)
		return
	}

	localAPIURL := "http://" + apitype.LocalAPIHost + "/localapi/" + path
	req, err := http.NewRequestWithContext(r.Context(), r.Method, localAPIURL, r.Body)
	if err != nil {
		http.Error(w, "failed to construct request", http.StatusInternalServerError)
		return
	}

	// Make request to tailscaled localapi.
	resp, err := s.lc.DoLocalRequest(req)
	if err != nil {
		http.Error(w, err.Error(), resp.StatusCode)
		return
	}
	defer resp.Body.Close()

	// Send response back to web frontend.
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// csrfKey returns a key that can be used for CSRF protection.
// If an error occurs during key creation, the error is logged and the active process terminated.
// If the server is running in CGI mode, the key is cached to disk and reused between requests.
// If an error occurs during key storage, the error is logged and the active process terminated.
func (s *Server) csrfKey() []byte {
	var csrfFile string

	// if running in CGI mode, try to read from disk, but ignore errors
	if s.cgiMode {
		confdir, err := os.UserConfigDir()
		if err != nil {
			confdir = os.TempDir()
		}

		csrfFile = filepath.Join(confdir, "tailscale", "web-csrf.key")
		key, _ := os.ReadFile(csrfFile)
		if len(key) == 32 {
			return key
		}
	}

	// create a new key
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatal("error generating CSRF key: %w", err)
	}

	// if running in CGI mode, try to write the newly created key to disk, and exit if it fails.
	if s.cgiMode {
		if err := os.Mkdir(filepath.Dir(csrfFile), 0700); err != nil && !os.IsExist(err) {
			log.Fatalf("unable to store CSRF key: %v", err)
		}
		if err := os.WriteFile(csrfFile, key, 0600); err != nil {
			log.Fatalf("unable to store CSRF key: %v", err)
		}
	}

	return key
}

// enforcePrefix returns a HandlerFunc that enforces a given path prefix is used in requests,
// then strips it before invoking h.
// Unlike http.StripPrefix, it does not return a 404 if the prefix is not present.
// Instead, it returns a redirect to the prefix path.
func enforcePrefix(prefix string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.Redirect(w, r, prefix, http.StatusFound)
			return
		}
		http.StripPrefix(prefix, h).ServeHTTP(w, r)
	}
}

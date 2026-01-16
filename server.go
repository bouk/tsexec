// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Minimal tailscale server for tsexec, derived from tailscale.com/tsnet.
// Stripped down to only what's needed for running with an external TUN device.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/tailscale/wireguard-go/tun"
	"tailscale.com/client/local"
	"tailscale.com/wgengine/router"
	"tailscale.com/control/controlclient"
	_ "tailscale.com/feature/c2n"
	_ "tailscale.com/feature/condregister/identityfederation"
	_ "tailscale.com/feature/condregister/oauthkey"
	_ "tailscale.com/feature/condregister/portmapper"
	_ "tailscale.com/feature/condregister/useproxy"
	"tailscale.com/envknob"
	"tailscale.com/health"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnauth"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/localapi"
	"tailscale.com/ipn/store"
	"tailscale.com/logpolicy"
	"tailscale.com/logtail"
	"tailscale.com/logtail/filch"
	"tailscale.com/net/memnet"
	"tailscale.com/net/netmon"
	"tailscale.com/net/tsaddr"
	"tailscale.com/net/tsdial"
	"tailscale.com/tsd"
	"tailscale.com/types/logger"
	"tailscale.com/types/logid"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/testenv"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
)

// Server is a minimal embedded Tailscale server that uses an external TUN device.
type Server struct {
	// Dir specifies the directory for state storage.
	Dir string

	// Hostname is the hostname to present to the control server.
	Hostname string

	// Ephemeral, if true, registers as an ephemeral node.
	Ephemeral bool

	// AuthKey is the auth key for node registration.
	AuthKey string

	// ControlURL optionally specifies the coordination server URL.
	ControlURL string

	// Tun is the TUN device to use. Required.
	Tun tun.Device

	// Router is the router to use for configuring routes in the network namespace.
	// If nil, a fake router is used.
	Router router.Router

	// ExitNode specifies an exit node to use for all traffic (hostname or IP).
	ExitNode string

	// Logf, if set, is used for logging.
	Logf logger.Logf

	initOnce         sync.Once
	initErr          error
	lb               *ipnlocal.LocalBackend
	sys              *tsd.System
	netstack         *netstack.Impl
	netMon           *netmon.Monitor
	rootPath         string
	hostname         string
	exitNodeHostname string // exit node hostname to resolve after connection
	shutdownCtx      context.Context
	shutdownCancel   context.CancelFunc
	localAPIListener *memnet.Listener
	localClient      *local.Client
	localAPIServer   *http.Server
	logbuffer        *filch.Filch
	logtail          *logtail.Logger
	logid            logid.PublicID
	dialer           *tsdial.Dialer

	mu     sync.Mutex
	closed bool
}

// Start connects the server to the tailnet.
func (s *Server) Start() error {
	hostinfo.SetPackage("tsexec")
	s.initOnce.Do(s.doInit)
	return s.initErr
}

// Up connects the server to the tailnet and waits until it is running.
func (s *Server) Up(ctx context.Context) (*ipnstate.Status, error) {
	if err := s.Start(); err != nil {
		return nil, fmt.Errorf("server.Up: %w", err)
	}

	watcher, err := s.localClient.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return nil, fmt.Errorf("server.Up: %w", err)
	}
	defer watcher.Close()

	for {
		n, err := watcher.Next()
		if err != nil {
			return nil, fmt.Errorf("server.Up: %w", err)
		}
		if n.ErrMessage != nil {
			return nil, fmt.Errorf("server.Up: backend: %s", *n.ErrMessage)
		}
		if st := n.State; st != nil {
			if *st == ipn.Running {
				status, err := s.localClient.Status(ctx)
				if err != nil {
					return nil, fmt.Errorf("server.Up: %w", err)
				}
				if len(status.TailscaleIPs) == 0 {
					return nil, errors.New("server.Up: running, but no ip")
				}
				return status, nil
			}
		}
	}
}

// Close stops the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*1e9) // 5 seconds
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.logtail != nil {
			s.logtail.Shutdown(ctx)
		}
		if s.logbuffer != nil {
			s.logbuffer.Close()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.localAPIServer != nil {
			s.localAPIServer.Shutdown(ctx)
		}
	}()

	if s.netstack != nil {
		s.netstack.Close()
		s.netstack = nil
	}
	if s.shutdownCancel != nil {
		s.shutdownCancel()
	}
	if s.lb != nil {
		s.lb.Shutdown()
	}
	if s.netMon != nil {
		s.netMon.Close()
	}
	if s.dialer != nil {
		s.dialer.Close()
	}
	if s.localAPIListener != nil {
		s.localAPIListener.Close()
	}

	wg.Wait()
	s.sys.Bus.Get().Close()
	s.closed = true
	return nil
}

func (s *Server) doInit() {
	s.shutdownCtx, s.shutdownCancel = context.WithCancel(context.Background())
	if err := s.start(); err != nil {
		s.initErr = fmt.Errorf("server: %w", err)
	}
}

func (s *Server) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

func (s *Server) getAuthKey() string {
	if v := s.AuthKey; v != "" {
		return v
	}
	if v := os.Getenv("TS_AUTHKEY"); v != "" {
		return v
	}
	return os.Getenv("TS_AUTH_KEY")
}

// TailscaleIPs returns the Tailscale IP addresses of this node.
func (s *Server) TailscaleIPs() (ip4, ip6 netip.Addr) {
	nm := s.lb.NetMap()
	if nm == nil {
		return
	}
	addrs := nm.GetAddresses()
	for i := range addrs.Len() {
		addr := addrs.At(i)
		ip := addr.Addr()
		if ip.Is6() {
			ip6 = ip
		} else {
			ip4 = ip
		}
	}
	return ip4, ip6
}

func (s *Server) start() (reterr error) {
	var closePool closeOnErrorPool
	defer closePool.closeAllIfError(&reterr)

	if s.Tun == nil {
		return errors.New("server: Tun device is required")
	}

	exe, err := os.Executable()
	if err != nil {
		switch runtime.GOOS {
		case "js", "wasip1", "ios":
			exe = "tsexec"
		default:
			return err
		}
	}
	prog := strings.TrimSuffix(strings.ToLower(filepath.Base(exe)), ".exe")

	s.hostname = s.Hostname
	if s.hostname == "" {
		s.hostname = prog
	}

	s.rootPath = s.Dir
	if s.rootPath == "" {
		confDir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		s.rootPath = filepath.Join(confDir, "tsexec-"+prog)
	}
	if err := os.MkdirAll(s.rootPath, 0700); err != nil {
		return err
	}

	tsLogf := func(format string, a ...any) {
		if s.logtail != nil {
			s.logtail.Logf(format, a...)
		}
		if s.Logf != nil {
			s.Logf(format, a...)
		}
	}

	sys := tsd.NewSystem()
	s.sys = sys
	if err := s.startLogger(&closePool, sys.HealthTracker.Get(), tsLogf); err != nil {
		return err
	}

	s.netMon, err = netmon.New(sys.Bus.Get(), tsLogf)
	if err != nil {
		return err
	}
	closePool.add(s.netMon)

	s.dialer = &tsdial.Dialer{Logf: tsLogf}
	s.dialer.SetBus(sys.Bus.Get())

	// Create the wireguard engine with our external TUN device
	eng, err := wgengine.NewUserspaceEngine(tsLogf, wgengine.Config{
		Tun:           s.Tun,    // Use the external TUN device
		Router:        s.Router, // Use the provided router (or nil for fake)
		EventBus:      sys.Bus.Get(),
		NetMon:        s.netMon,
		Dialer:        s.dialer,
		SetSubsystem:  sys.Set,
		ControlKnobs:  sys.ControlKnobs(),
		HealthTracker: sys.HealthTracker.Get(),
		Metrics:       sys.UserMetricsRegistry(),
	})
	if err != nil {
		return err
	}
	closePool.add(s.dialer)
	sys.Set(eng)
	sys.HealthTracker.Get().SetMetricsRegistry(sys.UserMetricsRegistry())

	tunWrapper := sys.Tun.Get()
	ns, err := netstack.Create(tsLogf, tunWrapper, eng, sys.MagicSock.Get(), s.dialer, sys.DNSManager.Get(), sys.ProxyMapper())
	if err != nil {
		return fmt.Errorf("netstack.Create: %w", err)
	}
	tunWrapper.Start()
	sys.Set(ns)

	// ProcessLocalIPs = false: Let ICMP and other traffic flow through the TUN device
	// ProcessSubnets = false: External traffic is handled by pasta, not netstack
	ns.ProcessLocalIPs = false
	ns.ProcessSubnets = false

	s.netstack = ns

	// Configure the dialer to use netstack for Tailscale IPs
	s.dialer.UseNetstackForIP = func(ip netip.Addr) bool {
		// Handle Tailscale service IPs (100.100.100.100 for MagicDNS)
		if ip == tsaddr.TailscaleServiceIP() || ip == tsaddr.TailscaleServiceIPv6() {
			return true
		}
		_, ok := eng.PeerForIP(ip)
		return ok
	}
	s.dialer.NetstackDialTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		v4, v6 := s.TailscaleIPs()
		var src netip.Addr
		if dst.Addr().Is6() {
			src = v6
		} else {
			src = v4
		}
		tcpConn, err := ns.DialContextTCPWithBind(ctx, src, dst)
		if err != nil {
			return nil, err
		}
		return tcpConn, nil
	}
	s.dialer.NetstackDialUDP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		v4, v6 := s.TailscaleIPs()
		var src netip.Addr
		if dst.Addr().Is6() {
			src = v6
		} else {
			src = v4
		}
		udpConn, err := ns.DialContextUDPWithBind(ctx, src, dst)
		if err != nil {
			return nil, err
		}
		return udpConn, nil
	}

	// Set up state storage
	stateFile := filepath.Join(s.rootPath, "tailscaled.state")
	s.logf("server running state path %s", stateFile)
	stateStore, err := store.New(tsLogf, stateFile)
	if err != nil {
		return err
	}
	sys.Set(stateStore)

	// Create the local backend
	loginFlags := controlclient.LoginDefault
	if s.Ephemeral {
		loginFlags = controlclient.LoginEphemeral
	}
	lb, err := ipnlocal.NewLocalBackend(tsLogf, s.logid, sys, loginFlags|controlclient.LocalBackendStartKeyOSNeutral)
	if err != nil {
		return fmt.Errorf("NewLocalBackend: %v", err)
	}
	lb.SetVarRoot(s.rootPath)
	s.logf("server starting with hostname %q, varRoot %q", s.hostname, s.rootPath)
	s.lb = lb

	if err := ns.Start(lb); err != nil {
		return fmt.Errorf("failed to start netstack: %w", err)
	}
	closePool.addFunc(func() { s.lb.Shutdown() })

	// Configure preferences
	prefs := ipn.NewPrefs()
	prefs.Hostname = s.hostname
	prefs.WantRunning = true
	prefs.ControlURL = s.ControlURL

	// Set exit node if specified
	if s.ExitNode != "" {
		if ip, err := netip.ParseAddr(s.ExitNode); err == nil {
			prefs.ExitNodeIP = ip
		} else {
			// Not an IP, treat as hostname - will be resolved after connection
			s.exitNodeHostname = s.ExitNode
		}
	}

	authKey := s.getAuthKey()
	err = lb.Start(ipn.Options{
		UpdatePrefs: prefs,
		AuthKey:     authKey,
	})
	if err != nil {
		return fmt.Errorf("starting backend: %w", err)
	}

	st := lb.State()
	if st == ipn.NeedsLogin || envknob.Bool("TSNET_FORCE_LOGIN") {
		s.logf("LocalBackend state is %v; running StartLoginInteractive...", st)
		if err := s.lb.StartLoginInteractive(s.shutdownCtx); err != nil {
			return fmt.Errorf("StartLoginInteractive: %w", err)
		}
	} else if authKey != "" {
		s.logf("Authkey is set; but state is %v.", st)
	}

	go s.printAuthURLLoop()

	// Set up local API handler
	lah := localapi.NewHandler(localapi.HandlerConfig{
		Actor:    ipnauth.Self,
		Backend:  lb,
		Logf:     tsLogf,
		LogID:    s.logid,
		EventBus: sys.Bus.Get(),
	})
	lah.PermitWrite = true
	lah.PermitRead = true

	lal := memnet.Listen("local-tailscaled.sock:80")
	s.localAPIListener = lal
	s.localClient = &local.Client{Dial: lal.Dial}
	s.localAPIServer = &http.Server{Handler: lah}
	s.lb.ConfigureWebClient(s.localClient)

	go func() {
		if err := s.localAPIServer.Serve(lal); err != nil && err != http.ErrServerClosed {
			s.logf("localapi serve error: %v", err)
		}
	}()
	closePool.add(s.localAPIListener)

	return nil
}

func (s *Server) startLogger(closePool *closeOnErrorPool, health *health.Tracker, tsLogf logger.Logf) error {
	if testenv.InTest() {
		return nil
	}
	cfgPath := filepath.Join(s.rootPath, "tailscaled.log.conf")
	lpc, err := logpolicy.ConfigFromFile(cfgPath)
	switch {
	case os.IsNotExist(err):
		lpc = logpolicy.NewConfig(logtail.CollectionNode)
		if err := lpc.Save(cfgPath); err != nil {
			return fmt.Errorf("logpolicy.Config.Save for %v: %w", cfgPath, err)
		}
	case err != nil:
		return fmt.Errorf("logpolicy.LoadConfig for %v: %w", cfgPath, err)
	}
	if err := lpc.Validate(logtail.CollectionNode); err != nil {
		return fmt.Errorf("logpolicy.Config.Validate for %v: %w", cfgPath, err)
	}
	s.logid = lpc.PublicID

	s.logbuffer, err = filch.New(filepath.Join(s.rootPath, "tailscaled"), filch.Options{ReplaceStderr: false})
	if err != nil {
		return fmt.Errorf("error creating filch: %w", err)
	}
	closePool.add(s.logbuffer)

	c := logtail.Config{
		Collection:   lpc.Collection,
		PrivateID:    lpc.PrivateID,
		Stderr:       io.Discard,
		Buffer:       s.logbuffer,
		CompressLogs: true,
		Bus:          s.sys.Bus.Get(),
		HTTPC:        &http.Client{Transport: logpolicy.NewLogtailTransport(logtail.DefaultHost, s.netMon, health, tsLogf)},
		MetricsDelta: clientmetric.EncodeLogTailMetricsDelta,
	}
	s.logtail = logtail.NewLogger(c, tsLogf)
	closePool.addFunc(func() { s.logtail.Shutdown(context.Background()) })
	return nil
}

func (s *Server) printAuthURLLoop() {
	for {
		if s.shutdownCtx.Err() != nil {
			return
		}
		n, err := s.localClient.WatchIPNBus(s.shutdownCtx, ipn.NotifyInitialState)
		if err != nil {
			return
		}
		for {
			ev, err := n.Next()
			if err != nil {
				break
			}
			if ev.BrowseToURL != nil {
				s.logf("AuthURL: %s", *ev.BrowseToURL)
			}
		}
		n.Close()
	}
}

// closeOnErrorPool tracks resources to close if an error occurs.
type closeOnErrorPool struct {
	closers []func()
}

func (p *closeOnErrorPool) add(c interface{ Close() error }) {
	p.closers = append(p.closers, func() { c.Close() })
}

func (p *closeOnErrorPool) addFunc(f func()) {
	p.closers = append(p.closers, f)
}

func (p *closeOnErrorPool) closeAllIfError(errp *error) {
	if *errp != nil {
		for _, c := range p.closers {
			c()
		}
	}
}

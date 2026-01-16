package main

import (
	"encoding/gob"
	"fmt"
	"io"
	"net"
	"net/netip"

	"tailscale.com/types/preftype"
	"tailscale.com/wgengine/router"
)

func init() {
	// Register types for gob encoding
	gob.Register(netip.Prefix{})
	gob.Register([]netip.Prefix{})
	gob.Register(preftype.NetfilterMode(0))
}

// RouterCommand represents a command to send to the remote router
type RouterCommand struct {
	Type   string         // "up", "set", "close", "ready"
	Config *router.Config // for "set" command
}

// RouterResponse is the response from a router command
type RouterResponse struct {
	Error string // empty if no error
}

// remoteRouter implements router.Router by sending commands to a child process
type remoteRouter struct {
	conn    net.Conn
	encoder *gob.Encoder
	decoder *gob.Decoder
}

func newRemoteRouter(conn net.Conn) *remoteRouter {
	return &remoteRouter{
		conn:    conn,
		encoder: gob.NewEncoder(conn),
		decoder: gob.NewDecoder(conn),
	}
}

func (r *remoteRouter) sendCommand(cmd RouterCommand) error {
	if err := r.encoder.Encode(cmd); err != nil {
		return fmt.Errorf("encode command: %w", err)
	}
	var resp RouterResponse
	if err := r.decoder.Decode(&resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("remote router: %s", resp.Error)
	}
	return nil
}

func (r *remoteRouter) Up() error {
	return r.sendCommand(RouterCommand{Type: "up"})
}

func (r *remoteRouter) Set(cfg *router.Config) error {
	return r.sendCommand(RouterCommand{Type: "set", Config: cfg})
}

func (r *remoteRouter) Close() error {
	err := r.sendCommand(RouterCommand{Type: "close"})
	r.conn.Close()
	return err
}

func (r *remoteRouter) Ready() error {
	return r.sendCommand(RouterCommand{Type: "ready"})
}

// routerServer handles router commands from the parent process
type routerServer struct {
	router  router.Router
	conn    net.Conn
	encoder *gob.Encoder
	decoder *gob.Decoder
	ready   chan struct{} // closed when "ready" command is received
}

func newRouterServer(rtr router.Router, conn net.Conn) *routerServer {
	return &routerServer{
		router:  rtr,
		conn:    conn,
		encoder: gob.NewEncoder(conn),
		decoder: gob.NewDecoder(conn),
		ready:   make(chan struct{}),
	}
}

func (s *routerServer) serve() error {
	for {
		var cmd RouterCommand
		if err := s.decoder.Decode(&cmd); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode command: %w", err)
		}

		var err error
		switch cmd.Type {
		case "up":
			err = s.router.Up()
		case "set":
			err = s.router.Set(cmd.Config)
		case "close":
			err = s.router.Close()
			// Send response before returning
			resp := RouterResponse{}
			if err != nil {
				resp.Error = err.Error()
			}
			s.encoder.Encode(resp)
			return nil
		case "ready":
			close(s.ready)
		default:
			err = fmt.Errorf("unknown command type: %s", cmd.Type)
		}

		resp := RouterResponse{}
		if err != nil {
			resp.Error = err.Error()
		}
		if err := s.encoder.Encode(resp); err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
	}
}

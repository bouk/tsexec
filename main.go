package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/sys/unix"
	"tailscale.com/health"
	"tailscale.com/net/netmon"
	"tailscale.com/util/eventbus"
	"tailscale.com/wgengine/router"
	_ "tailscale.com/wgengine/router/osrouter" // Register Linux router
)

const (
	tunMTU  = 1500
	tunName = "tailscale0"
)

func main() {
	// Check if we're running in inner mode (inside the pasta namespace)
	if len(os.Args) >= 2 && os.Args[1] == "--inner" {
		runInner()
		return
	}

	runOuter()
}

// runOuter is the main entry point - creates unix socket, launches pasta, receives TUN FD
func runOuter() {
	var (
		hostname = flag.String("hostname", "", "Hostname for Tailscale (default: tsexec-<random>)")
		authKey  = flag.String("auth-key", "", "Tailscale auth key (or TS_AUTHKEY env var)")
		verbose  = flag.Bool("verbose", false, "Enable verbose logging")
		stateDir = flag.String("state-dir", "", "Directory for Tailscale state (default: temp dir)")
		exitNode = flag.String("exit-node", "", "Use specified exit node for all traffic (hostname or IP)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: tsexec [options] <command> [args...]\n\n")
		fmt.Fprintf(os.Stderr, "Run a command with all traffic routed through Tailscale.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	// Set hostname with random suffix if not specified
	if *hostname == "" {
		*hostname = fmt.Sprintf("tsexec-%d", rand.Intn(10000))
	}

	// Get auth key from flag or environment
	if *authKey == "" {
		*authKey = os.Getenv("TS_AUTHKEY")
	}

	// Set up state directory
	dir := *stateDir
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "tsexec-*")
		if err != nil {
			log.Fatalf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(dir)
	}

	// Create a unix socket for passing the TUN FD and router commands
	sockPath := filepath.Join(dir, "tun.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("Failed to create unix socket: %v", err)
	}
	defer listener.Close()

	// Get our own executable path
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}

	// Set up signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Build the pasta command with our inner process
	// pasta -- tsexec --inner <socket-path> <command...>
	pastaArgs := []string{
		"pasta",
		"--config-net",
		"--",
		self,
		"--inner",
		sockPath, // Pass the socket path
	}
	pastaArgs = append(pastaArgs, flag.Args()...)

	// Create the child process
	cmd := exec.CommandContext(ctx, pastaArgs[0], pastaArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start pasta: %v", err)
	}

	// Accept connection from child
	listener.(*net.UnixListener).SetDeadline(time.Now().Add(30 * time.Second))
	conn, err := listener.Accept()
	if err != nil {
		log.Fatalf("Failed to accept connection: %v", err)
	}

	// Wait for the TUN FD from the child
	tunFD, err := recvFD(conn.(*net.UnixConn))
	if err != nil {
		log.Fatalf("Failed to receive TUN FD: %v", err)
	}

	if *verbose {
		log.Printf("Received TUN FD: %d", tunFD)
	}

	// Create tun.Device from the received FD using wireguard-go's function
	tunDev, tunDevName, err := tun.CreateUnmonitoredTUNFromFD(tunFD)
	if err != nil {
		log.Fatalf("Failed to create TUN device from FD: %v", err)
	}

	if *verbose {
		log.Printf("Created TUN device: %s", tunDevName)
	}

	// Create the remote router that sends commands to the child
	remoteRtr := newRemoteRouter(conn)

	// Create and start the tailscale server with the TUN device and remote router
	srv := &Server{
		Hostname:  *hostname,
		Dir:       dir,
		Ephemeral: true,
		AuthKey:   *authKey,
		Tun:       tunDev,
		Router:    remoteRtr,
		ExitNode:  *exitNode,
	}

	if *verbose {
		srv.Logf = log.Printf
	}

	// Start the server and wait for it to be ready
	status, err := srv.Up(ctx)
	if err != nil {
		log.Fatalf("Failed to bring up Tailscale: %v", err)
	}

	if *verbose {
		log.Printf("Tailscale connected as %s (%s)", status.Self.HostName, status.Self.TailscaleIPs[0])
	}

	// Signal the child that Tailscale is ready
	if err := remoteRtr.Ready(); err != nil {
		log.Fatalf("Failed to signal ready: %v", err)
	}

	// Handle signals
	go func() {
		<-sigCh
		cancel()
		srv.Close()
	}()

	// Wait for the child to exit
	err = cmd.Wait()
	srv.Close()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("Command failed: %v", err)
	}
}

// runInner runs inside the pasta namespace - creates TUN, sends FD to parent, handles router commands
func runInner() {
	// Args: tsexec --inner <socket-path> <command> [args...]
	if len(os.Args) < 4 {
		log.Fatalf("Usage: tsexec --inner <socket-path> <command> [args...]")
	}

	sockPath := os.Args[2]
	cmdArgs := os.Args[3:]

	// Connect to the parent's unix socket
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Fatalf("Failed to connect to parent socket: %v", err)
	}

	// Create the TUN device
	tunDev, err := tun.CreateTUN(tunName, tunMTU)
	if err != nil {
		log.Fatalf("Failed to create TUN device: %v", err)
	}

	// Get the file descriptor
	tunFile := tunDev.File()
	tunFD := int(tunFile.Fd())

	// Send the TUN FD to the parent
	if err := sendFD(conn.(*net.UnixConn), tunFD); err != nil {
		log.Fatalf("Failed to send TUN FD: %v", err)
	}

	// Create dependencies for router.New
	logf := log.Printf
	bus := eventbus.New()
	healthTracker := health.NewTracker(bus)
	netMon, err := netmon.New(bus, logf)
	if err != nil {
		log.Fatalf("Failed to create network monitor: %v", err)
	}

	// Create the router using the real router implementation
	rtr, err := router.New(logf, tunDev, netMon, healthTracker, bus)
	if err != nil {
		log.Fatalf("Failed to create router: %v", err)
	}

	// Create the router server
	rtrServer := newRouterServer(rtr, conn)

	// Handle router commands in a goroutine
	done := make(chan error, 1)
	go func() {
		done <- rtrServer.serve()
	}()

	// Wait for the parent to signal that Tailscale is ready
	<-rtrServer.ready

	// Now start the user command
	userCmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	userCmd.Stdin = os.Stdin
	userCmd.Stdout = os.Stdout
	userCmd.Stderr = os.Stderr

	if err := userCmd.Start(); err != nil {
		log.Fatalf("Failed to start command: %v", err)
	}

	// Wait for user command in a goroutine
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- userCmd.Wait()
	}()

	// Wait for either router server to finish or user command to exit
	var exitCode int
	select {
	case err := <-done:
		log.Printf("DEBUG: router server finished: %v", err)
		if err != nil {
			log.Printf("Router server error: %v", err)
		}
		// Router connection closed, kill user command if still running
		userCmd.Process.Signal(syscall.SIGTERM)
		<-cmdDone
	case err := <-cmdDone:
		// User command exited
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		// Exit immediately - os.Exit will close everything
		os.Exit(exitCode)
	}
	os.Exit(exitCode)
}

// sendFD sends a file descriptor over a unix connection
func sendFD(conn *net.UnixConn, fd int) error {
	f, err := conn.File()
	if err != nil {
		return err
	}
	defer f.Close()
	rights := unix.UnixRights(fd)
	return unix.Sendmsg(int(f.Fd()), []byte{0}, rights, nil, 0)
}

// recvFD receives a file descriptor from a unix connection
func recvFD(conn *net.UnixConn) (int, error) {
	f, err := conn.File()
	if err != nil {
		return -1, err
	}
	defer f.Close()

	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgLen(4))

	_, oobn, _, _, err := unix.Recvmsg(int(f.Fd()), buf, oob, 0)
	if err != nil {
		return -1, fmt.Errorf("recvmsg: %w", err)
	}

	if oobn == 0 {
		return -1, fmt.Errorf("no OOB data received")
	}

	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("ParseSocketControlMessage: %w", err)
	}

	if len(scms) == 0 {
		return -1, fmt.Errorf("no control messages")
	}

	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, fmt.Errorf("ParseUnixRights: %w", err)
	}

	if len(fds) == 0 {
		return -1, fmt.Errorf("no file descriptors")
	}

	return fds[0], nil
}

# tsexec

Run commands with all network traffic routed through Tailscale, without root privileges.

## Overview

tsexec creates an isolated network namespace using [pasta](https://passt.top/) and routes traffic through Tailscale. This allows you to run any command with its network traffic going through your Tailscale network, useful for:

- Accessing Tailscale-only resources from scripts
- Running commands through a Tailscale exit node
- Isolating application traffic to the Tailscale network

## Requirements

- Linux (uses TUN devices and network namespaces)
- [pasta](https://passt.top/) installed and in PATH
- A Tailscale auth key (for ephemeral node registration)

## Usage

```bash
tsexec [options] -- <command> [args...]
```

### Options

- `-hostname <name>` - Hostname for the Tailscale node (default: tsexec-<random>)
- `-auth-key <key>` - Tailscale auth key (or set `TS_AUTHKEY` env var)
- `-auth-key-file <path>` - Path to file containing Tailscale auth key
- `-state-dir <dir>` - Directory for Tailscale state (default: temp dir)
- `-exit-node <host>` - Route all traffic through specified exit node
- `-verbose` - Enable verbose logging

### Examples

```bash
# Ping a Tailscale peer
tsexec -auth-key tskey-auth-xxx -- ping -c3 100.100.100.100

# Run curl through Tailscale
tsexec -auth-key tskey-auth-xxx -- curl http://my-server.tailnet.ts.net

# Use an exit node for all traffic
tsexec -auth-key tskey-auth-xxx -exit-node exit-node-name -- curl https://example.com
```

## How It Works

tsexec uses a two-process architecture to route traffic through Tailscale without requiring root privileges:

```
┌─────────────────────────────────────────────────────────────────┐
│ Parent Process (host namespace)                                 │
│                                                                 │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────┐     │
│  │ Tailscale    │────▶│ WireGuard    │────▶│ magicsock    │     │
│  │ control      │     │ engine       │     │ (UDP)        │     │
│  └──────────────┘     └──────┬───────┘     └──────────────┘     │
│                              │                                  │
│                              │ read/write packets               │
│                              ▼                                  │
│                     TUN FD (received from child)                │
└────────────────────────────────┬────────────────────────────────┘
                                 │ unix socket
                    ─────────────┼───────────────────────────────
                                 │ namespace boundary (pasta)
┌────────────────────────────────┼────────────────────────────────┐
│ Child Process (pasta namespace)│                                │
│                                ▼                                │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │ TUN device (tailscale0)                                     ││
│  │ - Created by child, FD passed to parent                     ││
│  │ - Routes configured by Linux router                         ││
│  └─────────────────────────────────────────────────────────────┘│
│                                ▲                                │
│                                │ routed traffic                 │
│                     ┌──────────┴──────────┐                     │
│                     │ User Command        │                     │
│                     │ (ping, curl, etc)   │                     │
│                     └─────────────────────┘                     │
└─────────────────────────────────────────────────────────────────┘
```

### Detailed Flow

1. **Namespace Creation**: The parent process launches pasta, which creates a new network namespace. Inside this namespace, tsexec runs in "inner" mode.

2. **TUN Device Setup**: The inner process creates a TUN device (`tailscale0`) and passes the file descriptor to the parent via a unix socket using `SCM_RIGHTS`.

3. **Router Setup**: The inner process creates a Linux router (using Tailscale's `wgengine/router`) that configures routes and iptables rules in the namespace. Router commands are forwarded from the parent via the unix socket.

4. **Tailscale Connection**: The parent process runs a minimal Tailscale server using the TUN device. It connects to the Tailscale control plane and establishes WireGuard tunnels to peers.

5. **Ready Signal**: Once Tailscale is connected, the parent sends a "ready" signal to the child, which then starts the user command.

6. **Traffic Flow**:
   - User command sends packets (e.g., ICMP ping)
   - Linux kernel routes packets to `tailscale0` (based on configured routes)
   - Parent reads packets from TUN FD
   - WireGuard encrypts and sends via magicsock
   - Responses come back through WireGuard
   - Parent writes decrypted packets to TUN FD
   - Kernel delivers packets to user command

7. **Cleanup**: When the user command exits, the child process terminates, which causes pasta to clean up the namespace. The parent logs out the ephemeral Tailscale node.

### Why This Architecture?

- **No root required**: pasta uses user namespaces, so no elevated privileges are needed
- **Full protocol support**: Unlike SOCKS proxies, this approach supports TCP, UDP, and ICMP and doesn't require tools to support proxies.

## Installation

```bash
go install bou.ke/tsexec@latest
```

Make sure `pasta` is installed on your system.

## Building from Source

```bash
# Build for Linux
GOOS=linux GOARCH=amd64 go build -o tsexec .
```

## License

BSD-3-Clause (same as Tailscale)

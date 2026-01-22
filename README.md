# tsexec

Run commands with all traffic routed through Tailscale, without root privileges.

## How it works

tsexec uses [pasta](https://passt.top/passt/about/) to create a network namespace where a normal `tailscaled` runs. Pasta provides the necessary capabilities (CAP_NET_ADMIN in the user namespace) for tailscaled to create a TUN device directly.

A mount namespace is used to bind the tailscale socket to the default path, so `tailscale` commands work normally inside the namespace.

## Usage

```bash
tsexec [tailscale up flags] -- [command]
```

### Examples

```bash
# Run a shell connected to your tailnet
tsexec --authkey=tskey-xxx --

# Run curl through an exit node
tsexec --authkey=tskey-xxx --exit-node=us-server -- curl ifconfig.me

# Ping a machine on your tailnet
tsexec --authkey=tskey-xxx -- ping 100.x.x.x
```

All flags before `--` are passed to `tailscale up`. Everything after `--` is the command to run.

## Installation

### Nix

```bash
nix run github:bouk/tsexec -- --authkey=tskey-xxx -- curl ifconfig.me
```

Or add to your flake:

```nix
{
  inputs.tsexec.url = "github:bouk/tsexec";
}
```

### Manual

Requires: `pasta` (from passt), `tailscale`, `tailscaled`, `unshare`, `bash`

```bash
./tsexec --authkey=tskey-xxx -- curl ifconfig.me
```

## License

BSD-3-Clause

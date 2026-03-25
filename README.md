# tfm (Terminal For Mac)

A simple, lightweight terminal multiplexer written in Go, inspired by `tmux`. It features built-in session saving and persistent background daemons.

## Features

- **Session Multiplexing:** Create multiple named terminal sessions.
- **Detach/Attach:** Detach from a session (`Ctrl+B`, `d`) and re-attach later.
- **Persistent Daemon:** The server runs in the background even if you close your terminal.
- **Session Saving:** Save session names and working directories to disk for automatic restoration on next start.
- **Automatic Daemon Management:** The client automatically starts the background daemon if it's not already running.

## Installation

### From Source (Requires Go)

```bash
go install github.com/Chetas-Patil/terminalformac@latest
```

Alternatively, clone the repo and build:

```bash
git clone https://github.com/Chetas-Patil/terminalformac.git
cd terminalformac
go build -o tfm main.go
sudo mv tfm /usr/local/bin/
```

## Usage

### Create a new session
```bash
tfm new -s <session_name>
```

### Detach from session
Press `Ctrl+B` followed by `d`.

### List active sessions
```bash
tfm ls
```

### Attach to a session
```bash
tfm attach -t <session_name>
```

### Save current sessions to disk
```bash
tfm save
```
*Note: Saved sessions are stored in `~/.tfm/sessions.json` and are automatically reloaded when the `tfm` daemon starts.*

### Kill a session
```bash
tfm kill -t <session_name>
```

## Shortcuts
- `Ctrl+B`, `d`: Detach from current session.
- `Ctrl+B`, `Ctrl+B`: Send a literal `Ctrl+B` to the inner terminal.

## How it works
`tfm` uses a Unix Domain Socket (located at `~/.tfm/tfm.sock`) to communicate between the client and a background daemon. Each session runs a dedicated PTY (pseudo-terminal) that remains active as long as the shell process inside it is alive.

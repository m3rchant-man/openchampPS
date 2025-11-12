## OpenChamp Server

Lightweight Go WebSocket lobby, matchmaking, and game server launcher for the OpenChamp project.
This repository contains the server components (Web API, WebSocket manager, port manager) and helper scripts used to run and develop locally.

Key folders
- `gameserver/` — Where the Gameserver should be placed post-build and the game server image is built.
- `_scripts/` — helper scripts for varoius operating systems setup and use this repository.

Prerequisites
- Go 1.20+ installed and available on PATH
- Podman (Windows/Linux) or Container (MacOS)
- Git for development workflow

Quick start
1. Start Postgres Container with your engine
2. Build the server:
```powershell
cd <repo-root>
mkdir openchamp-server
go build -o openchamp-server ./...
```
3. Run the server:
```powershell
.\openchamp-server
```

## Local development (without Docker)
- Ensure a Postgres instance is reachable and environment/config values are correct (`.env`). Review `config/config.go` for connection configuration and environment variable names.
- `go run main.go` to start the server


## Modules
General Module information
### Config Module
- Manages the Database Connection and general configuration chagnes

### PortManager Module
- Manages the deployment and decommisioning of GameServers, Ports, and the ingame queue

### WebAPI
- Handles the HTTP API requests

### WSM Module
- WebSocket Manager (WSM) handles the active websocket connections between all users. (Definitely a bottleneck at scale, will be rewritten at some point)

## Running game servers
- The `portmanager` package manages assigning available ports for spawned game server processes. The `gameserver/` folder contains shipped binaries and helper scripts used by the project; keep these files if you rely on local process spawning.

Testing and validation
- Quick build check:

```powershell
cd <repo-root>
go build ./...
```

- There are no automated unit tests bundled currently; add tests under the relevant packages where you change behavior.

## Contribution guide
- Fork the repository and create feature branches (use descriptive names like `feature/upgrade-db` or `fix/session-leak`).
- Run `go build ./...` and verify your changes compile before opening a PR.
- Keep changes small and focused. Include a short description and rationale in your PR.

### Code style and quality
- Follow typical Go conventions (gofmt, idiomatic error handling). We recommend running `gofmt -w .` before committing.

### Contact
- If you have questions about running or developing the server, please open an issue on the repository with details and platform information (OS, Go version).

---
Thank you for being a part of the OpenChamp Community!

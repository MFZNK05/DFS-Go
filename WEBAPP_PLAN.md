# DFS-Go: Web App & Product Architecture Plan

---
## APPENDIX: Product Architecture — Campus Resource Hub
### (Read after Sprint 8 distributed core is complete)
---

## The Core Problem You're Solving

No always-on server. ~500–2000 students. Laptops go offline constantly. Files must be available during college hours when students are active. This is exactly the BitTorrent problem — solved decades ago. The answer is **availability through replication across many peers**, not through one always-on server.

The key insight: **you don't need 100% uptime. You need files to survive as long as enough peers have them.** With N=3 replication across an active department, if even 3 students who have the file are online (guaranteed during college hours), everyone else can download it.

---

## Architecture: Two-Tier Model (Realistic)

```
Tier 0 — Storage nodes (run the DFS daemon)
  ─ Your laptop (while you keep it on)           ← you bootstrap the network
  ─ A Raspberry Pi 4 / old laptop / lab PC       ← permanent anchor, ~$50-$70 one-time
  ─ Optionally: 1-2 other volunteers later
  ─ These machines store ALL the files, participate in P2P ring
  ─ Normal students do NOT need to be in this tier

Tier 1 — Browser clients (everyone else)
  ─ Any student opens the web app URL in their browser
  ─ Logs in, uploads a file → file goes to Tier 0 nodes
  ─ Downloads a file → file streamed from Tier 0 nodes
  ─ No daemon, no installation, no background process
  ─ Works on phone, laptop, tablet — any device on college LAN
```

**Answer to your question:** YES, you alone running the daemon (initially on your laptop, then on a Pi) is enough to make this work for all other students. They just use the browser. You are the infrastructure. Other students are pure users.

**Raspberry Pi 4 (2GB RAM, ~$50) is more than enough for:**
- 3 DFS storage nodes (different ports on same Pi, or 3 Pi's later)
- PostgreSQL for metadata
- Go REST API
- Nginx serving the Next.js frontend
- Handling 500+ concurrent browser clients

**Concrete day-1 setup:**
```
Your laptop: dfs start --port 3000 --port 3001   (2 nodes)
Pi (later):  dfs start --port 3000 --port 3001 --port 3002  (3 permanent nodes)
Students:    open browser → http://192.168.1.X → use the app
```
With replication factor N=3 across your nodes, every file has 3 copies on your Pi. If a student uploads while you're offline, they just can't upload yet. When your Pi is online (always), uploads always work.

---

## Graceful Shutdown Protocol (Critical for Laptop Nodes)

When a student closes their laptop or quits the app, the node must **not just vanish**. It must:

1. **Signal intent:** Send `MessageLeaving{Addr: self}` to all connected peers via gossip
2. **Trigger re-replication:** Peers receiving this immediately check which keys that node owned → re-replicate those keys to N other live nodes
3. **Wait for confirmation:** Node waits up to 10s for re-replication to complete before shutting down
4. **Update membership:** ClusterState marks node as `StateLeft` (not Dead) — clean departure

This means files survive as long as re-replication happens faster than laptops close — which it will at N=3 with 50+ online peers.

```go
// New message type (Sprint 3 / membership package)
type MessageLeaving struct {
    Addr      string
    Timestamp int64
}

// Server.GracefulShutdown()
//  1. broadcast MessageLeaving to all peers
//  2. wait up to 10s for Rebalancer.OnNodeLeft(self) to complete
//  3. close(s.quitch)
```

---

## Data Persistence Strategy (No Server)

Since there is no always-on server, data durability depends entirely on how many copies exist across peer laptops.

**Storage per student node:** Each DFS node stores its share of the replicated data locally (in `<port>_network/` directory). A student with 10GB free disk space contributes 10GB to the pool. With N=3 replication, the effective pool is `totalDisk / 3`.

**What happens when ALL 3 replica holders are offline simultaneously?**
- File is temporarily unavailable (acceptable per your requirement)
- When any one of the 3 comes back online, it re-seeds the file
- The file is NOT lost — it's on disk, just unreachable

**To minimize this:** Upload popular files with N=5 or N=7 replication. Unpopular files at N=3. The uploader chooses replication factor.

---

## Bootstrapping the Network (No Central Server)

How does a new student find anyone to connect to?

**Solution: Hardcoded seed list + gossip**

```go
// cmd/start.go — seeds are well-known stable peers (e.g., a department PC that's usually on)
// Even if seeds are offline, the list is tried and gossip fills in the rest
var defaultSeeds = []string{
    "192.168.1.10:3000",  // Lab PC 1 (usually on during college hours)
    "192.168.1.11:3000",  // Lab PC 2
    "192.168.1.50:3000",  // A volunteer's always-on laptop
}
```

If ALL seeds are offline when a student starts the app:
- Student's node starts in isolated mode
- App shows "Connecting to network..." spinner
- Retries seed connection every 30s
- When any seed comes online, gossip propagates the full peer list within seconds

**mDNS local discovery (enhancement):** Use `github.com/grandcat/zeroconf` to discover peers on the same LAN subnet automatically — no seed list needed on the same WiFi network. Every DFS node announces itself via mDNS. New nodes find peers within 1–2 seconds on the same network.

---

## Web App Architecture (Product Layer)

### What students interact with

```
Browser (any device on LAN)
    │
    ▼ HTTP/HTTPS
┌─────────────────────────────┐
│   API Gateway (Go + Gin)    │  ← runs on one of the peer machines
│   Port 8080                 │    (or any student's machine that's on)
│                             │
│  /api/auth/...              │
│  /api/files/...             │
│  /api/search?q=...          │
│  /api/subjects              │
└────────┬────────────────────┘
         │ direct function calls (same process)
         ▼
┌─────────────────────────────┐
│   DFS Server (this project) │
│   Port 3000 (P2P)           │
│   StoreData / GetData       │
└─────────────────────────────┘
         │
         ▼
┌─────────────────────────────┐
│   PostgreSQL (metadata)     │  ← runs locally on the gateway machine
│   - users table             │
│   - resources table         │
│   - full-text search index  │
└─────────────────────────────┘
```

**Key design:** The API gateway is NOT a single point of failure for the files. Files are distributed across all DFS nodes. The gateway just handles HTTP, auth, and metadata. If the gateway machine goes offline, another student can start the gateway on their machine using the same PostgreSQL data (if Postgres is also replicated — or use SQLite which is a single file easy to copy).

---

## Database Schema

```sql
-- users
CREATE TABLE users (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email        TEXT UNIQUE NOT NULL,       -- must end in @college.edu
    name         TEXT NOT NULL,
    dept         TEXT NOT NULL,              -- "CSE", "ECE", "MECH" etc.
    year         INT,                        -- 1, 2, 3, 4
    role         TEXT DEFAULT 'student',     -- 'student', 'moderator', 'admin'
    password_hash TEXT NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

-- resources (metadata index — actual files in DFS)
CREATE TABLE resources (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dfs_key      TEXT UNIQUE NOT NULL,       -- key used in StoreData/GetData
    title        TEXT NOT NULL,
    description  TEXT,
    subject      TEXT,                       -- "Data Structures", "DBMS"
    file_type    TEXT,                       -- "pdf", "video", "ppt", "doc", "zip"
    size_bytes   BIGINT,
    uploader_id  UUID REFERENCES users(id),
    dept         TEXT,
    year         INT,                        -- which year is this for?
    tags         TEXT[],                     -- ["exam", "2024", "unit-3"]
    downloads    INT DEFAULT 0,
    repl_factor  INT DEFAULT 3,
    uploaded_at  TIMESTAMPTZ DEFAULT NOW(),
    search_vec   TSVECTOR                    -- auto-updated by trigger below
);

CREATE INDEX resources_search_idx ON resources USING GIN(search_vec);
CREATE INDEX resources_dept_idx ON resources(dept);
CREATE INDEX resources_subject_idx ON resources(subject);

-- trigger: auto-update search_vec on insert/update
CREATE OR REPLACE FUNCTION update_search_vec() RETURNS trigger AS $$
BEGIN
    NEW.search_vec := to_tsvector('english',
        coalesce(NEW.title, '') || ' ' ||
        coalesce(NEW.subject, '') || ' ' ||
        coalesce(NEW.description, '') || ' ' ||
        coalesce(array_to_string(NEW.tags, ' '), '')
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER resources_search_trigger
BEFORE INSERT OR UPDATE ON resources
FOR EACH ROW EXECUTE FUNCTION update_search_vec();
```

---

## REST API Endpoints (Full List)

```
Auth:
  POST /api/auth/signup       body: {email, name, dept, year, password}
  POST /api/auth/login        body: {email, password} → {access_token, refresh_token}
  POST /api/auth/refresh      body: {refresh_token} → {access_token}
  POST /api/auth/logout

Files:
  POST   /api/files           multipart: file + {title, subject, dept, year, tags, repl_factor}
                              → calls StoreData, inserts into resources table
  GET    /api/files           ?dept=CSE&subject=OS&type=pdf&year=3&sort=recent&page=1
  GET    /api/files/:id       → metadata only
  GET    /api/files/:id/download → calls GetData, streams file to browser
  DELETE /api/files/:id       → uploader or admin only, calls Store.Remove

Search:
  GET /api/search?q=sorting+algorithms&dept=CSE
      → PostgreSQL tsvector query, returns ranked results

Subjects:
  GET /api/subjects?dept=CSE  → distinct subjects in that dept

Users:
  GET  /api/users/me
  GET  /api/users/:id/uploads
  PUT  /api/users/me          → update name, year

Admin:
  GET    /api/admin/users
  DELETE /api/admin/users/:id
  PUT    /api/admin/users/:id/role
  GET    /api/admin/nodes     → DFS ring members, health status
```

---

## Auth: JWT Implementation

```go
// Access token: 15 min, signed with HS256
// Refresh token: 7 days, stored in DB (allows revocation)
// Both in HTTP-only cookies (XSS protection)

type Claims struct {
    UserID string `json:"sub"`
    Email  string `json:"email"`
    Dept   string `json:"dept"`
    Role   string `json:"role"`
    jwt.RegisteredClaims
}

// Middleware: authRequired(roles ...string)
//  1. extract cookie "access_token"
//  2. parse + verify JWT
//  3. check role if specified
//  4. set user info in Gin context
```

**College email gate:** On signup, verify email ends in `@<college_domain>` (configurable). Send verification email if SMTP is available, otherwise admin approves manually.

---

## Frontend: Next.js Pages & Components

```
Pages:
  /                   → landing page (login if not authed)
  /login              → login form
  /signup             → signup form
  /dashboard          → recent uploads, trending, quick-upload widget
  /browse             → resource grid with sidebar filters
  /upload             → drag-drop zone + metadata form
  /files/[id]         → file detail: preview + download button + uploader info
  /search             → search results page
  /profile            → my uploads + download history
  /admin              → user management table (admin only)

Key Components:
  <FileCard>          → thumbnail, title, subject tag, size, downloads, uploader
  <FilePreview>       → PDF.js for PDF, <video> for MP4/WebM, <img> for images
  <UploadZone>        → react-dropzone, shows progress bar during upload
  <SubjectFilter>     → sidebar checkboxes for subject/year/type
  <SearchBar>         → debounced input → /api/search
  <NodeStatus>        → shows ring size, peer count (admin widget)
```

---

## Upload Flow (End to End)

```
1. Student drags file into <UploadZone>
2. Frontend: POST /api/files (multipart form)
3. API handler:
   a. parse metadata from form fields
   b. generate dfs_key = sha256(title + uploader_id + timestamp)
   c. call server.StoreData(dfs_key, fileReader)     ← DFS stores + replicates
   d. INSERT INTO resources (dfs_key, title, ...)    ← PostgreSQL records metadata
   e. return {id, dfs_key, title}
4. Frontend shows "Upload complete, replicated to N nodes"
```

---

## Download Flow (End to End)

```
1. Student clicks Download on a file card
2. Frontend: GET /api/files/:id/download
3. API handler:
   a. SELECT dfs_key FROM resources WHERE id = :id
   b. UPDATE resources SET downloads = downloads + 1
   c. reader, err := server.GetData(dfs_key)    ← DFS fetches from ring
   d. stream reader → http.ResponseWriter with Content-Disposition header
4. Browser receives file stream
```

---

## mDNS Auto-Discovery Implementation

```go
// Peer2Peer/mdns/mdns.go
// Dep: github.com/grandcat/zeroconf

const serviceType = "_dfsgo._tcp"

type MDNSService struct {
    server   *zeroconf.Server
    resolver *zeroconf.Resolver
    onFound  func(addr string)   // called when peer discovered on LAN
    port     int
    stopCh   chan struct{}
}

// Announce(port int) — broadcasts this node's presence on LAN
//   zeroconf.Register("dfsgo-<port>", serviceType, "local.", port, nil, nil)

// Discover(onFound func(string)) — watches for other DFS nodes on LAN
//   resolver.Browse(ctx, serviceType, "local.", entries)
//   for entry := range entries: onFound(entry.AddrIPv4[0] + ":" + entry.Port)

// Integration in MakeServer:
//   mdns := mdns.New(port, func(addr string) { s.tcpTransport.Dial(addr) })
//   mdns.Announce()
//   mdns.Discover()
```

---

## Graceful Shutdown (OS Signal Handler)

```go
// cmd/daemon.go — trap SIGTERM/SIGINT
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
go func() {
    <-sigCh
    log.Println("Shutting down gracefully...")
    server.GracefulShutdown()   // broadcasts leaving, waits for re-replication
    os.Exit(0)
}()
```

---

## Deployment: Docker Compose (Single Command)

```yaml
# docker-compose.yml
version: "3.9"
services:
  dfs-node:
    build: .
    ports:
      - "3000:3000"    # P2P port
      - "4000:4000"    # health endpoint
    environment:
      - DFS_ENCRYPTION_KEY=${DFS_ENCRYPTION_KEY}
      - DFS_SEEDS=192.168.1.10:3000,192.168.1.11:3000
    volumes:
      - dfs-data:/app/data
    restart: unless-stopped

  api:
    build: ./api
    ports:
      - "8080:8080"
    environment:
      - DATABASE_URL=postgres://dfs:dfs@postgres:5432/dfsgo
      - JWT_SECRET=${JWT_SECRET}
      - DFS_NODE=dfs-node:3000
    depends_on:
      - postgres
      - dfs-node

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: dfs
      POSTGRES_PASSWORD: dfs
      POSTGRES_DB: dfsgo
    volumes:
      - pg-data:/var/lib/postgresql/data

  nginx:
    image: nginx:alpine
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf
      - ./frontend/out:/usr/share/nginx/html   # Next.js static export

volumes:
  dfs-data:
  pg-data:
```

**Students install nothing.** They open a browser and go to `http://192.168.1.X` (your Pi's LAN IP). The Pi runs the full compose stack permanently. Your laptop can run an extra DFS node while on, adding redundancy, but is not required.

**Raspberry Pi setup (one-time):**
```bash
# On Pi:
git clone <repo>
cp .env.example .env   # set DFS_ENCRYPTION_KEY, JWT_SECRET
docker-compose up -d   # starts 3 DFS nodes + API + PostgreSQL + Nginx
```
Cost: ~$50-70 one-time for Pi + SD card. Electricity: ~3W, negligible.

**Your laptop (optional extra node while on):**
```bash
dfs start --port 4000 --seeds 192.168.1.X:3000
# adds another storage node while your laptop is on
# gracefully leaves when you shut down
```

---

## Recommended Implementation Order (Post Sprint 8)

```
Phase A — API Layer (2-3 weeks)
  1. api/ package: Go + Gin REST server
  2. PostgreSQL schema + goose migrations
  3. Auth: signup/login/JWT middleware
  4. /files upload + download endpoints
  5. /search endpoint

Phase B — Frontend (2-3 weeks)
  1. Next.js + Tailwind setup
  2. Login/signup pages
  3. Dashboard + browse page with FileCard grid
  4. Upload page with drag-drop + progress
  5. File detail page with preview

Phase C — Networking Enhancements (1 week)
  1. mDNS auto-discovery (Peer2Peer/mdns/)
  2. Graceful shutdown with re-replication signal
  3. Configurable seed list in config file

Phase D — Docker + Deploy (1 week)
  1. Dockerfile for DFS node + API
  2. docker-compose.yml
  3. nginx.conf for reverse proxy
  4. Deploy on 2-3 student machines as anchors

Phase E — Polish (ongoing)
  1. PDF.js preview
  2. Admin panel
  3. Download count, trending resources
  4. Department/subject stats
```

---

## Final Tech Stack

| Layer | Tech | Reason |
|-------|------|--------|
| P2P Storage | DFS-Go (this project) | The whole point |
| REST API | Go + Gin | Same language, embed DFS directly |
| Auth | JWT + bcrypt | Simple, no external deps |
| Metadata DB | PostgreSQL | Full-text search built-in |
| DB Migrations | goose | Simple SQL migration tool |
| Frontend | Next.js + Tailwind | SSR, fast, good ecosystem |
| File preview | PDF.js + native `<video>` | No extra server needed |
| LAN discovery | mDNS (zeroconf) | Zero config peer discovery |
| Container | Docker Compose | One command deploy |
| Reverse proxy | Nginx | Static files + API routing |
| Off-campus (optional) | Cloudflare Tunnel | Free, zero config |

---

## Context

DFS-Go is a college P2P resource-sharing file system (audio, video, PDFs, PPTs, docs). After Sprint 0 (18 bug fixes) and Sprint 1 (consistent hashing, `Cluster/hashring/`), the system can route keys to target nodes but lacks: write confirmation, failure handling, dynamic discovery, consistency guarantees, chunking (OOM risk on large files), and eventual convergence.

This document is the **complete master plan** for Sprints 2–8. Each sprint is self-contained with exact struct fields, method signatures, step-by-step logic, integration changes, new message types, CLI flags, and tests. After completing all sprints the distributed core is done and the web app layer (REST API, auth, frontend) can begin.

**Modularity rule:** Every topic lives in its own sub-package with no circular imports.

---

## Current State (post Sprint 0 + 1)

```
Server struct fields:
  peerLock    sync.RWMutex
  peers       map[string]peer2peer.Peer
  serverOpts  ServerOpts          // tcpTransport *TCPTransport, metaData MetadataStore,
                                  // Encryption *EncryptionService, ReplicationFactor int
  Store       *storage.Store      // WriteStream, ReadStream, Has, Remove
  HashRing    *hashring.HashRing  // GetNodes(key,n), AddNode, RemoveNode, Members
  quitch      chan struct{}
  pendingFile map[string]chan io.Reader
  mu          sync.Mutex

Wire protocol: [0x1 control byte][gob Message] OR [0x2 control byte][raw stream bytes]
Message types: MessageStoreFile{Key,Size,EncryptedKey}, MessageGetFile{Key}, MessageLocalFile{Key,Size}
CAS storage: SHA-256 dirs + MD5 filename
Encryption: AES-GCM streaming, 4MB chunks, [4B size][12B nonce][ciphertext+tag]
```

---


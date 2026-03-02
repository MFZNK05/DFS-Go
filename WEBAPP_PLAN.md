# DFS-Go: Web App & Product Architecture Plan

---
## APPENDIX: Product Architecture — Campus Resource Hub
### (Read after Sprint 8 distributed core is complete)
---

## Architectural Foundations

This document answers every critical design question head-on before diving into implementation. Each section maps a concrete doubt to a concrete decision with rationale.

---

## Pillar 1: Network Topology — "Browser as a Peer" Resolved

**The Doubt:** Can all web app users actually be peers in the network?

**The Answer: No. And they don't need to be.**

Browsers cannot open raw TCP/QUIC listen sockets. A React/Next.js app running in Chrome cannot join the DFS-Go hash ring. This is a fundamental browser sandbox limitation.

**Decision: Gateway Model (Option A)**

We deploy a small cluster of Go DFS nodes on backend infrastructure (Raspberry Pi, lab PCs, or a single always-on machine). The web app is a thin HTTP client that talks to an API gateway co-located with a DFS node. Students never install anything — they open a browser.

**Why not the Local Daemon Model?** Requiring 500+ students to download, install, and background-run a Go binary is a non-starter for adoption. The Gateway Model lets us launch with zero friction.

---

## Pillar 2: Identity, Access Control & Namespaces

**The Doubt:** How do we handle different scopes — departments, societies, 1-on-1, and private files?

**The Answer: The P2P layer stays dumb. All access control lives in the Metadata Service.**

The DFS nodes only know about `chunk:hash` keys. They enforce zero access policy. The PostgreSQL metadata layer maps human identity → file ownership → group membership → access grants.

### Scope Model

```
┌─────────────────────────────────────────────────┐
│                   Visibility                     │
├─────────────────────────────────────────────────┤
│  public    │ Anyone in the college can see/download │
│  dept      │ Only students in same department       │
│  group     │ Only members of a specific group       │
│  private   │ Only the uploader + explicit grantees  │
└─────────────────────────────────────────────────┘
```

### Groups (Departments, Societies, Ad-Hoc)

```sql
CREATE TABLE groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    type        TEXT NOT NULL CHECK (type IN ('department', 'society', 'custom')),
    created_by  UUID REFERENCES users(id),
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE group_members (
    group_id    UUID REFERENCES groups(id) ON DELETE CASCADE,
    user_id     UUID REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT DEFAULT 'member' CHECK (role IN ('member', 'admin', 'owner')),
    joined_at   TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (group_id, user_id)
);

-- Departments are pre-seeded groups of type='department'
-- INSERT INTO groups (name, type) VALUES ('CSE', 'department'), ('ECE', 'department'), ...
-- Students are auto-added to their department group on signup
```

### File Access Control

```sql
-- resources table gets a visibility column
ALTER TABLE resources ADD COLUMN visibility TEXT DEFAULT 'public'
    CHECK (visibility IN ('public', 'dept', 'group', 'private'));

-- For group/private files, explicit grants
CREATE TABLE file_shares (
    resource_id UUID REFERENCES resources(id) ON DELETE CASCADE,
    grantee_type TEXT NOT NULL CHECK (grantee_type IN ('user', 'group')),
    grantee_id  UUID NOT NULL,  -- user.id or group.id
    granted_by  UUID REFERENCES users(id),
    granted_at  TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (resource_id, grantee_type, grantee_id)
);
```

### Access Check Middleware (Go)

```go
func canAccessFile(userID, resourceID uuid.UUID) bool {
    r := db.GetResource(resourceID)
    switch r.Visibility {
    case "public":
        return true
    case "dept":
        return db.UserDept(userID) == r.Dept
    case "group":
        // Check if user is in ANY group that has a share grant
        return db.HasGroupAccess(userID, resourceID)
    case "private":
        return r.UploaderID == userID || db.HasDirectShare(userID, resourceID)
    }
    return false
}
```

**1-on-1 sharing:** The uploader sets `visibility=private`, then creates a `file_shares` row with `grantee_type=user, grantee_id=<friend>`. The friend can now download.

**Society files:** A society president creates a group, adds members. Uploads with `visibility=group` and a `file_shares` row pointing to the group. All members can download.

---

## Pillar 3: Production Security & Key Management

**The Doubt:** Currently testing by sharing encrypted keys and mTLS manually. How does this work in production?

**The Answer: Hybrid Encryption + automated CA provisioning.**

### Current Problem

Right now, `DFS_ENCRYPTION_KEY` is a single env var. Every node uses the same master key. Every chunk is decryptable by anyone who runs the daemon. This is symmetric encryption with a shared secret — fine for a P2P mesh where all nodes are trusted infrastructure, but it means the **API gateway** (which has the key) is the sole gatekeeper.

### Production Security Design

**Layer 1 — Node-to-Node (mTLS): Already Solved**

Your `Crypto/tls.go` already implements full mTLS with ECDSA P-256. In production:
- Generate CA once on the gateway/Pi
- Copy CA cert + key to all DFS nodes (or automate via config management)
- Each node generates its own node cert signed by the CA
- Peer connections are encrypted + mutually authenticated

**Layer 2 — File Encryption: Keep Symmetric, Guard at the API**

For a campus deployment with trusted infrastructure nodes, the current model is sufficient:
- All DFS nodes share the same `DFS_ENCRYPTION_KEY`
- The API gateway is the only path to files (students can't talk to DFS nodes directly)
- Access control is enforced in the API middleware before calling `StoreData`/`GetData`

Files are encrypted at rest on all nodes (protects against disk theft), and the API enforces who can request decryption.

**Why not full hybrid encryption (per-user public keys)?**
- Adds massive complexity (key distribution, key rotation, recovery)
- For a college LAN where you control the infrastructure, it's overkill
- The threat model is "protect data at rest" + "enforce access in the API", not "defend against compromised infrastructure nodes"

**Layer 3 — API Security**
- JWT tokens (HS256, 15-min access + 7-day refresh)
- bcrypt password hashing
- College email domain validation on signup
- Rate limiting on auth endpoints
- HTTPS via Nginx + self-signed cert (or Let's Encrypt if routable)

### If You Later Need Per-User Encryption (Hybrid Model)

```
Upload:
  1. Generate random AES-256 key for this file (already done)
  2. Encrypt file with AES key (already done)
  3. For each authorized user/group member:
     - Encrypt the AES key with their RSA/ECDH public key
     - Store encrypted_key_for_user in file_shares table
  4. Store encrypted file in DFS (no master key needed)

Download:
  1. Look up user's encrypted_key entry in file_shares
  2. User decrypts AES key with their private key (client-side)
  3. Decrypt file stream with AES key
```

This is the Signal/WhatsApp model. Defer this to v2 — the symmetric approach works for v1.

---

## Pillar 4: File Mutability & Versioning

**The Doubt:** If a user updates a study guide, the hash changes entirely. Is it a new file?

**The Answer: Version chain in metadata, immutable chunks in DFS.**

DFS chunks are content-addressed (CAS). Updating a file means new chunks with new hashes. The metadata layer tracks the version history.

```sql
ALTER TABLE resources ADD COLUMN version     INT DEFAULT 1;
ALTER TABLE resources ADD COLUMN parent_id   UUID REFERENCES resources(id);
ALTER TABLE resources ADD COLUMN is_latest   BOOLEAN DEFAULT true;

-- When a user "updates" a file:
-- 1. Set old resource: is_latest = false
-- 2. Insert new resource: parent_id = old.id, version = old.version + 1, is_latest = true
-- 3. New resource gets a new dfs_key (because content changed)
-- 4. Old chunks are NOT deleted (see Garbage Collection below)
```

### API Changes

```
PUT /api/files/:id          multipart: new file + optional metadata updates
                            → creates new version, preserves history

GET /api/files/:id/versions → returns [{version: 1, uploaded_at, size}, {version: 2, ...}]
GET /api/files/:id?version=1 → download specific version
```

### Implementation

```go
func updateFile(resourceID uuid.UUID, newFile io.Reader, meta ResourceMeta) error {
    old := db.GetResource(resourceID)

    // 1. Store new file in DFS
    newKey := generateDFSKey(meta.Title, userID, time.Now())
    server.StoreData(newKey, newFile)

    // 2. Mark old as not-latest
    db.Exec("UPDATE resources SET is_latest = false WHERE id = $1", old.ID)

    // 3. Insert new version
    db.Exec(`INSERT INTO resources (dfs_key, title, ..., version, parent_id, is_latest)
             VALUES ($1, $2, ..., $3, $4, true)`,
        newKey, meta.Title, old.Version+1, old.ID)

    return nil
}
```

### Default Behavior

- Browse/search only shows `is_latest = true` resources
- File detail page shows a "Version History" section
- Downloading always gets the latest unless a specific version is requested

---

## Pillar 5: Distributed Garbage Collection

**The Doubt:** If metadata is deleted, how do DFS nodes know it's safe to delete physical chunks?

**The Answer: Reference counting + periodic sweep.**

### The Problem

```
resources table:  DELETE WHERE id = X   (metadata gone)
DFS nodes:        chunk:abc123 still on disk (orphaned, wasting space)
```

### Solution: Two-Phase GC

**Phase 1 — Soft Delete + Reference Counting**

```sql
ALTER TABLE resources ADD COLUMN deleted_at TIMESTAMPTZ DEFAULT NULL;

-- "Delete" = soft delete (set deleted_at, keep dfs_key reference)
UPDATE resources SET deleted_at = NOW() WHERE id = :id;

-- A chunk_refs view counts how many live resources reference each manifest
CREATE VIEW chunk_refs AS
SELECT dfs_key, COUNT(*) FILTER (WHERE deleted_at IS NULL) AS live_refs
FROM resources
GROUP BY dfs_key;
```

**Phase 2 — GC Sweep (Background Job)**

```go
// Runs every 24 hours on the API gateway
func gcSweep() {
    // 1. Find all soft-deleted resources older than 30 days with zero live refs
    stale := db.Query(`
        SELECT r.id, r.dfs_key FROM resources r
        JOIN chunk_refs cr ON cr.dfs_key = r.dfs_key
        WHERE r.deleted_at < NOW() - INTERVAL '30 days'
          AND cr.live_refs = 0
    `)

    for _, r := range stale {
        // 2. Delete chunks from DFS nodes
        manifest := server.GetManifest(r.DFSKey)
        if manifest != nil {
            for _, chunk := range manifest.Chunks {
                server.DeleteChunk(chunk.StorageKey)  // new RPC to delete from all replicas
            }
        }
        server.DeleteManifest(r.DFSKey)

        // 3. Hard delete metadata
        db.Exec("DELETE FROM resources WHERE id = $1", r.ID)
    }
}
```

**Why 30 days?** Grace period allows:
- Undo accidental deletes
- Version history to remain accessible
- DFS rebalancing to settle before chunks are removed

### New DFS Message Type

```go
type MessageDeleteChunk struct {
    Key string
}
// Handler: server.Store.Remove(key), metadata.Delete(key)
// Propagated to all ring-responsible nodes
```

---

## Pillar 6: Search Architecture

**The Doubt:** You can't search inside encrypted P2P chunks.

**The Answer: Correct. All search is in PostgreSQL.**

Already addressed in the existing plan. The `tsvector` index on `resources` handles title, description, subject, and tags. Encrypted chunk data is never searched — the metadata layer is the search surface.

```sql
-- Already defined: resources.search_vec TSVECTOR with GIN index
-- Query:
SELECT * FROM resources
WHERE search_vec @@ plainto_tsquery('english', $1)
  AND is_latest = true
  AND deleted_at IS NULL
  AND (visibility = 'public' OR <access_check>)
ORDER BY ts_rank(search_vec, plainto_tsquery('english', $1)) DESC
LIMIT 20 OFFSET $2;
```

---

## Pillar 7: Bootstrap & Seed Nodes

**The Doubt:** Who runs the initial seed nodes for 24/7 uptime?

**The Answer: You do. One Raspberry Pi.**

Already addressed in the existing plan. The Pi runs 2-3 DFS nodes + API + PostgreSQL + Nginx via Docker Compose. This is the permanent anchor. Student laptops can optionally run additional DFS nodes for extra redundancy but are never required.

The gossip protocol + mDNS ensures that any node that comes online automatically discovers and joins the existing cluster within seconds.

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
-- =====================================================
-- USERS
-- =====================================================
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

-- =====================================================
-- GROUPS (departments, societies, custom study groups)
-- =====================================================
CREATE TABLE groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    type        TEXT NOT NULL CHECK (type IN ('department', 'society', 'custom')),
    description TEXT,
    created_by  UUID REFERENCES users(id),
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE group_members (
    group_id    UUID REFERENCES groups(id) ON DELETE CASCADE,
    user_id     UUID REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT DEFAULT 'member' CHECK (role IN ('member', 'admin', 'owner')),
    joined_at   TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (group_id, user_id)
);

-- Department groups are auto-seeded on bootstrap:
-- INSERT INTO groups (name, type) VALUES ('CSE', 'department'), ('ECE', 'department'), ...
-- Students are auto-added to their dept group on signup

-- =====================================================
-- RESOURCES (metadata index — actual files in DFS)
-- =====================================================
CREATE TABLE resources (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dfs_key      TEXT NOT NULL,              -- key used in StoreData/GetData (NOT unique: versions share lineage)
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

    -- Visibility / access control
    visibility   TEXT DEFAULT 'public'
                 CHECK (visibility IN ('public', 'dept', 'group', 'private')),

    -- Versioning
    version      INT DEFAULT 1,
    parent_id    UUID REFERENCES resources(id),   -- previous version
    is_latest    BOOLEAN DEFAULT true,

    -- Soft delete for GC
    deleted_at   TIMESTAMPTZ DEFAULT NULL,

    uploaded_at  TIMESTAMPTZ DEFAULT NOW(),
    search_vec   TSVECTOR                    -- auto-updated by trigger below
);

CREATE UNIQUE INDEX resources_dfs_key_version_idx ON resources(dfs_key, version);
CREATE INDEX resources_search_idx ON resources USING GIN(search_vec);
CREATE INDEX resources_dept_idx ON resources(dept);
CREATE INDEX resources_subject_idx ON resources(subject);
CREATE INDEX resources_latest_idx ON resources(is_latest) WHERE is_latest = true;
CREATE INDEX resources_deleted_idx ON resources(deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX resources_parent_idx ON resources(parent_id);

-- =====================================================
-- FILE SHARES (access grants for group/private files)
-- =====================================================
CREATE TABLE file_shares (
    resource_id  UUID REFERENCES resources(id) ON DELETE CASCADE,
    grantee_type TEXT NOT NULL CHECK (grantee_type IN ('user', 'group')),
    grantee_id   UUID NOT NULL,   -- references users.id or groups.id
    granted_by   UUID REFERENCES users(id),
    granted_at   TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (resource_id, grantee_type, grantee_id)
);

-- =====================================================
-- CHUNK REFERENCE COUNTING (for garbage collection)
-- =====================================================
CREATE VIEW chunk_refs AS
SELECT dfs_key, COUNT(*) FILTER (WHERE deleted_at IS NULL) AS live_refs
FROM resources
GROUP BY dfs_key;

-- =====================================================
-- FULL-TEXT SEARCH TRIGGER
-- =====================================================
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
  POST   /api/files           multipart: file + {title, subject, dept, year, tags, repl_factor, visibility}
                              → calls StoreData, inserts into resources table
  GET    /api/files           ?dept=CSE&subject=OS&type=pdf&year=3&sort=recent&page=1
                              → only returns is_latest=true, deleted_at IS NULL, access-checked
  GET    /api/files/:id       → metadata only (with access check)
  GET    /api/files/:id/download → calls GetData, streams file to browser (with access check)
  PUT    /api/files/:id       multipart: new file + optional metadata updates → creates new version
  DELETE /api/files/:id       → soft delete (uploader or admin only)

Versioning:
  GET /api/files/:id/versions → returns [{version, uploaded_at, size_bytes, dfs_key}, ...]
  GET /api/files/:id/versions/:v/download → download specific version

Sharing & Access Control:
  POST   /api/files/:id/share   body: {grantee_type: "user"|"group", grantee_id}
  DELETE /api/files/:id/share   body: {grantee_type, grantee_id}
  GET    /api/files/:id/shares  → list current grants (owner only)

Search:
  GET /api/search?q=sorting+algorithms&dept=CSE&visibility=public
      → PostgreSQL tsvector query, returns ranked results (access-filtered)

Groups:
  POST   /api/groups           body: {name, type: "society"|"custom", description}
  GET    /api/groups           ?type=society → list groups user belongs to
  GET    /api/groups/:id       → group detail + member list
  POST   /api/groups/:id/members   body: {user_id}   → add member (group admin only)
  DELETE /api/groups/:id/members/:uid              → remove member
  GET    /api/groups/:id/files → files shared with this group

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
  POST   /api/admin/gc        → trigger manual garbage collection sweep
  GET    /api/admin/gc/status  → orphan chunk count, last sweep time
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
  /browse             → resource grid with sidebar filters (dept, subject, year, type)
  /upload             → drag-drop zone + metadata form + visibility picker
  /files/[id]         → file detail: preview + download + version history + sharing panel
  /search             → search results page
  /groups             → my groups (societies, study groups)
  /groups/[id]        → group detail: members + shared files
  /profile            → my uploads + download history + shared-with-me
  /admin              → user management + node status + GC controls (admin only)

Key Components:
  <FileCard>          → thumbnail, title, subject tag, size, downloads, visibility badge, uploader
  <FilePreview>       → PDF.js for PDF, <video> for MP4/WebM, <img> for images
  <UploadZone>        → react-dropzone, shows progress bar during upload
  <VisibilityPicker>  → radio: public/dept/group/private + group selector dropdown
  <ShareDialog>       → search users by name/email, grant access to private files
  <VersionHistory>    → timeline of file versions with download links
  <SubjectFilter>     → sidebar checkboxes for subject/year/type
  <SearchBar>         → debounced input → /api/search
  <GroupCard>         → group name, member count, file count
  <NodeStatus>        → shows ring size, peer count, health (admin widget)
```

---

## Upload Flow (End to End)

```
1. Student drags file into <UploadZone>, selects:
   - Title, Subject, Dept, Year, Tags
   - Visibility: public / dept / group / private
   - If group: picks which group(s) from dropdown
   - If private: optionally share with specific users
2. Frontend: POST /api/files (multipart form)
3. API handler:
   a. Verify JWT → extract user identity
   b. Parse metadata from form fields
   c. Generate dfs_key = sha256(title + uploader_id + timestamp)
   d. Call server.StoreData(dfs_key, fileReader)     ← DFS stores + replicates
   e. INSERT INTO resources (dfs_key, title, visibility, ...)
   f. If visibility=group: INSERT file_shares for each selected group
   g. If visibility=private + shares: INSERT file_shares for each grantee
   h. Return {id, dfs_key, title, visibility}
4. Frontend shows "Upload complete, replicated to N nodes"
```

---

## Download Flow (End to End)

```
1. Student clicks Download on a file card
2. Frontend: GET /api/files/:id/download
3. API handler:
   a. SELECT resource FROM resources WHERE id = :id AND deleted_at IS NULL
   b. Access check: canAccessFile(user, resource) → 403 if denied
   c. UPDATE resources SET downloads = downloads + 1
   d. reader, err := server.GetData(dfs_key)    ← DFS fetches from ring
   e. Stream reader → http.ResponseWriter with:
      - Content-Disposition: attachment; filename="original_name.pdf"
      - Content-Type: detected from file_type
4. Browser receives file stream
```

---

## Update (Versioning) Flow

```
1. Student opens file detail page, clicks "Upload New Version"
2. Frontend: PUT /api/files/:id (multipart form with new file)
3. API handler:
   a. Verify user is original uploader (or admin)
   b. Fetch old resource record
   c. Set old.is_latest = false
   d. Store new file: server.StoreData(newKey, fileReader)
   e. INSERT new resource: version = old.version + 1, parent_id = old.id, is_latest = true
   f. Copy visibility + shares from old version
4. Frontend shows "Version 2 uploaded. Previous version preserved."
   - Version history sidebar appears on file detail page
```

---

## Soft Delete + GC Flow

```
1. Student (or admin) clicks Delete on a file
2. Frontend: DELETE /api/files/:id
3. API handler:
   a. Verify ownership or admin role
   b. UPDATE resources SET deleted_at = NOW() WHERE id = :id
   c. File disappears from browse/search immediately
   d. DFS chunks remain on disk (30-day grace period)

4. Background GC (runs daily at 3 AM):
   a. Query: resources WHERE deleted_at < NOW() - 30 days AND live_refs = 0
   b. For each: fetch manifest → delete all chunks from DFS ring → hard delete row
   c. Log: "GC sweep: removed N files, freed M bytes"
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
Phase A — API Foundation (2-3 weeks)
  1. api/ package: Go + Gin REST server, embedded alongside DFS server
  2. PostgreSQL schema + goose migrations (all tables: users, groups, resources, file_shares)
  3. Auth: signup (college email gate) / login / JWT middleware
  4. /files upload endpoint → StoreData + INSERT resources
  5. /files download endpoint → access check + GetData
  6. /search endpoint → tsvector query with access filtering

Phase B — Access Control (1-2 weeks)
  1. Groups CRUD: create/join/leave, auto-dept-group on signup
  2. Visibility on upload: public/dept/group/private
  3. file_shares CRUD: share/unshare/list
  4. canAccessFile middleware enforced on GET/download

Phase C — Frontend Core (2-3 weeks)
  1. Next.js + Tailwind setup
  2. Login/signup pages
  3. Dashboard + browse page with FileCard grid + SubjectFilter
  4. Upload page with drag-drop + VisibilityPicker
  5. File detail page with preview + ShareDialog

Phase D — Versioning & GC (1 week)
  1. PUT /files/:id → version chain creation
  2. VersionHistory component on file detail page
  3. Soft delete on DELETE /files/:id
  4. GC sweep background job (daily cron)
  5. MessageDeleteChunk message type in DFS

Phase E — Groups & Social (1 week)
  1. Groups page: list my groups, create new
  2. Group detail: manage members, view shared files
  3. Society file feeds

Phase F — Networking Enhancements (1 week)
  1. mDNS auto-discovery (Peer2Peer/mdns/)
  2. Graceful shutdown with re-replication signal
  3. Configurable seed list in config file

Phase G — Docker + Deploy (1 week)
  1. Dockerfile for DFS node + API (single binary)
  2. docker-compose.yml (DFS + API + PostgreSQL + Nginx)
  3. nginx.conf for reverse proxy
  4. Deploy on Raspberry Pi as permanent 24/7 anchor

Phase H — Polish (ongoing)
  1. PDF.js preview for file detail page
  2. Admin panel: user management, node health, GC controls
  3. Download count, trending resources, department stats
  4. Rate limiting, request logging, error monitoring
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


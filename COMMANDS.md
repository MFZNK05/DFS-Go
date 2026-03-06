# DFS-Go Command Reference

## Identity Setup (required once per machine)

```bash
# Create identity (required before any upload/download)
dfs identity init --alias <your-name>

# Show current identity
dfs identity show

# Custom identity path (useful for testing multiple nodes on same machine)
DFS_IDENTITY_PATH=/path/to/identity.json dfs identity init --alias <name>
```

## Starting Nodes

```bash
# Single node
dfs start --port :3000 --replication 2

# Join existing cluster (local or remote)
dfs start --port :3001 --replication 2 --peer <ip>:3000

# Multiple peers
dfs start --port :3002 --replication 2 --peer <ip1>:3000 --peer <ip2>:3001
```

## Upload / Download

```bash
# Plaintext upload
dfs upload --file <path> --name <key> --node :3000

# Plaintext download (own file)
dfs download --name <key> --output <path> --node :3000

# Download someone else's file
dfs download --name <key> --from <alias> --output <path> --node :3000

# ECDH encrypted upload (share with specific users)
dfs upload --file <path> --name <key> --share-with bob --node :3000
dfs upload --file <path> --name <key> --share-with bob,charlie --node :3000

# ECDH encrypted download
dfs download --name <key> --from alice --output <path> --node :3001

# Share using fingerprint directly (if alias is ambiguous)
dfs upload --file <path> --name <key> --share-with-key <fingerprint> --node :3000
dfs download --name <key> --from-key <fingerprint> --output <path> --node :3001
```

## Local Multi-Node Testing (same machine, different ports)

```bash
# Terminal 1 - Alice
mkdir -p /tmp/dfs-test
DFS_IDENTITY_PATH=/tmp/dfs-test/alice.json dfs identity init --alias alice
DFS_IDENTITY_PATH=/tmp/dfs-test/alice.json dfs start --port :3000 --replication 2

# Terminal 2 - Bob
DFS_IDENTITY_PATH=/tmp/dfs-test/bob.json dfs identity init --alias bob
DFS_IDENTITY_PATH=/tmp/dfs-test/bob.json dfs start --port :3001 --replication 2 --peer 127.0.0.1:3000

# Terminal 3 - Charlie
DFS_IDENTITY_PATH=/tmp/dfs-test/charlie.json dfs identity init --alias charlie
DFS_IDENTITY_PATH=/tmp/dfs-test/charlie.json dfs start --port :3002 --replication 2 --peer 127.0.0.1:3000

# Upload from Alice
DFS_IDENTITY_PATH=/tmp/dfs-test/alice.json dfs upload --file myfile.pdf --name notes --node :3000

# Download from Bob
DFS_IDENTITY_PATH=/tmp/dfs-test/bob.json dfs download --name notes --from alice --output downloaded.pdf --node :3001

# ECDH encrypted
DFS_IDENTITY_PATH=/tmp/dfs-test/alice.json dfs upload --file secret.pdf --name secret --share-with bob --node :3000
DFS_IDENTITY_PATH=/tmp/dfs-test/bob.json dfs download --name secret --from alice --output secret.pdf --node :3001

# Verify integrity
sha256sum myfile.pdf downloaded.pdf
```

## Docker Container Testing

```bash
# Build image
docker build -f Dockerfile.test -t dfs-test .

# Create network + identity volumes
docker network create dfs-net
docker volume create dfs-alice-id
docker volume create dfs-bob-id

# Init identities
docker run --rm -v dfs-alice-id:/identity -e DFS_IDENTITY_PATH=/identity/identity.json dfs-test identity init --alias alice
docker run --rm -v dfs-bob-id:/identity -e DFS_IDENTITY_PATH=/identity/identity.json dfs-test identity init --alias bob

# Start nodes
docker run -d --name dfs-alice --network dfs-net \
  -v dfs-alice-id:/identity \
  -e DFS_IDENTITY_PATH=/identity/identity.json \
  dfs-test start --port :3000 --replication 2

docker run -d --name dfs-bob --network dfs-net \
  -v dfs-bob-id:/identity \
  -e DFS_IDENTITY_PATH=/identity/identity.json \
  dfs-test start --port :3000 --replication 2 --peer dfs-alice:3000

# Upload from Alice container
docker cp myfile.pdf dfs-alice:/data/myfile.pdf
docker exec dfs-alice sh -c 'DFS_IDENTITY_PATH=/identity/identity.json dfs upload --file /data/myfile.pdf --name notes --node :3000'

# Download from Bob container
docker exec dfs-bob sh -c 'DFS_IDENTITY_PATH=/identity/identity.json dfs download --name notes --from alice --output /data/notes.pdf --node :3000'
docker cp dfs-bob:/data/notes.pdf ./downloaded.pdf

# Verify
sha256sum myfile.pdf downloaded.pdf

# Cleanup
docker rm -f dfs-alice dfs-bob
docker network rm dfs-net
docker volume rm dfs-alice-id dfs-bob-id
```

## Hybrid Testing (1 local + Docker containers)

```bash
# Find host IP on Docker network
docker network create dfs-hybrid
docker network inspect dfs-hybrid --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}'
# Output example: 172.22.0.1  (use this as HOST_IP below)

# Local node
dfs identity init --alias alice
dfs start --port :3000 --replication 2

# Container node
docker volume create dfs-bob-id
docker run --rm -v dfs-bob-id:/identity -e DFS_IDENTITY_PATH=/identity/identity.json \
  dfs-test identity init --alias bob
docker run -d --name dfs-bob --network dfs-hybrid \
  -v dfs-bob-id:/identity \
  -e DFS_IDENTITY_PATH=/identity/identity.json \
  dfs-test start --port :3000 --replication 2 --peer 172.22.0.1:3000

# Upload locally, download from container
dfs upload --file myfile.pdf --name notes --node :3000
docker exec dfs-bob sh -c 'DFS_IDENTITY_PATH=/identity/identity.json dfs download --name notes --from alice --output /data/notes.pdf --node :3000'

# Upload from container, download locally
docker exec dfs-bob sh -c 'DFS_IDENTITY_PATH=/identity/identity.json dfs upload --file /data/notes.pdf --name bob-notes --node :3000'
dfs download --name bob-notes --from bob --output ./bob-notes.pdf --node :3000
```

## Physical Network Testing (multiple laptops)

```bash
# On each machine first:
dfs identity init --alias <unique-name>

# Machine 1 (seed node) - find your LAN IP first
ip addr show | grep "inet " | grep -v 127.0.0.1
# e.g. 192.168.1.42
dfs start --port :3000 --replication 3

# Machine 2
dfs start --port :3000 --replication 3 --peer 192.168.1.42:3000

# Machine 3
dfs start --port :3000 --replication 3 --peer 192.168.1.42:3000

# Machines 4-10 (can peer with ANY running node - gossip handles full discovery)
dfs start --port :3000 --replication 3 --peer 192.168.1.42:3000

# Upload from any machine
dfs upload --file lecture.pdf --name lecture-notes --node :3000

# Download from any other machine
dfs download --name lecture-notes --from <uploader-alias> --output lecture.pdf --node :3000

# Encrypted sharing between two specific people
dfs upload --file exam-answers.pdf --name answers --share-with bob --node :3000
# Only Bob can download:
dfs download --name answers --from alice --output answers.pdf --node :3000
```

## Health / Monitoring

```bash
# Health endpoint (port = node port + 1000)
curl http://localhost:4000/health

# Prometheus metrics
curl http://localhost:4000/metrics

# Profiling
curl http://localhost:4000/debug/pprof/
```

## Troubleshooting

```bash
# Check if node is running
ls -la /tmp/dfs-3000.sock

# Check Docker container logs
docker logs <container-name>

# Verify two files match
sha256sum file1.pdf file2.pdf

# Check firewall (physical network)
# QUIC uses UDP - make sure UDP port 3000 is open
sudo ufw allow 3000/udp                              # Ubuntu
sudo firewall-cmd --add-port=3000/udp --permanent     # Fedora/RHEL

# Windows (if applicable)
# netsh advfirewall firewall add rule name="DFS" dir=in action=allow protocol=UDP localport=3000
```

## Notes

- QUIC runs over UDP - ensure UDP port is open on all machines' firewalls
- Each node needs a unique alias (`dfs identity init --alias <name>`)
- Only one `--peer` is needed to join - gossip auto-discovers the rest
- Storage key format: `<fingerprint>/<name>` - same name from different users won't collide
- `--replication` sets how many copies of each chunk exist in the cluster

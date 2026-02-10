import java.util.*;

public class ConsistentHashRing<T> {
    private final SortedMap<Long, T> ring = new TreeMap<>();
    private final Map<T, List<Long>> nodeToHashes = new HashMap<>();
    private final HashFunction hashFunction;
    private final int virtualNodesPerServer;
    private final int replicationFactor;
    
    public ConsistentHashRing(int virtualNodes, int replicationFactor) {
        this.virtualNodesPerServer = virtualNodes;
        this.replicationFactor = replicationFactor;
        this.hashFunction = new MD5HashFunction();  // Use MD5
    }
    
    // ===== Core Operations =====
    
    /**
     * Add a server to the ring
     */
    public void addServer(T server) {
        List<Long> hashes = new ArrayList<>();
        
        // Create virtual nodes for this server
        for (int i = 0; i < virtualNodesPerServer; i++) {
            Long hash = hashFunction.hash(
                server.toString() + ":vnode:" + i
            );
            ring.put(hash, server);
            hashes.add(hash);
        }
        
        nodeToHashes.put(server, hashes);
        
        // Trigger migration of affected keys
        triggerRebalancing(server);
    }
    
    /**
     * Remove a server from the ring
     */
    public void removeServer(T server) {
        List<Long> hashes = nodeToHashes.remove(server);
        if (hashes != null) {
            for (Long hash : hashes) {
                ring.remove(hash);
            }
        }
        
        // Trigger re-replication of affected keys
        triggerRecovery(server);
    }
    
    /**
     * Get N replicas for a key
     */
    public List<T> getReplicas(String key, int n) {
        List<T> replicas = new ArrayList<>();
        Set<T> seenServers = new HashSet<>();
        
        Long keyHash = hashFunction.hash(key);
        
        // Find successor
        SortedMap<Long, T> tailMap = ring.tailMap(keyHash);
        Iterator<Map.Entry<Long, T>> iterator;
        
        if (tailMap.isEmpty()) {
            iterator = ring.entrySet().iterator();
        } else {
            iterator = tailMap.entrySet().iterator();
        }
        
        // Collect N different servers
        while (seenServers.size() < n && !ring.isEmpty()) {
            if (!iterator.hasNext()) {
                iterator = ring.entrySet().iterator();
            }
            
            T server = iterator.next().getValue();
            if (seenServers.add(server)) {
                replicas.add(server);
            }
        }
        
        return replicas;
    }
    
    /**
     * Get primary server for a key
     */
    public T getPrimaryServer(String key) {
        return getReplicas(key, 1).get(0);
    }
    
    // ===== Helper Methods =====
    
    private void triggerRebalancing(T newServer) {
        // Identify keys that now belong to newServer
        // Migrate them from old owners
        
        // For each virtual node of newServer:
        for (Long newNodeHash : nodeToHashes.get(newServer)) {
            // Find predecessor node
            SortedMap<Long, T> headMap = ring.headMap(newNodeHash);
            if (!headMap.isEmpty()) {
                Long predecessorHash = headMap.lastKey();
                T predecessorServer = ring.get(predecessorHash);
                
                // Keys between predecessor and this node migrate
                // to this node
                
                // In production, this triggers async migration jobs
            }
        }
    }
    
    private void triggerRecovery(T failedServer) {
        // For each key that was on failedServer,
        // re-replicate to new servers
        
        // In production, this scans all data and re-replicates
    }
    
    // ===== Statistics =====
    
    public void printRingStatistics() {
        Map<T, Integer> serverLoad = new HashMap<>();
        
        for (T server : nodeToHashes.keySet()) {
            serverLoad.put(server, 0);
        }
        
        // Simulate distribution of 1M keys
        for (int i = 0; i < 1_000_000; i++) {
            String key = "key:" + i;
            T primary = getPrimaryServer(key);
            serverLoad.put(primary, serverLoad.get(primary) + 1);
        }
        
        System.out.println("Load Distribution:");
        for (Map.Entry<T, Integer> entry : serverLoad.entrySet()) {
            double percentage = (entry.getValue() * 100.0) / 1_000_000;
            System.out.println(entry.getKey() + ": " + percentage + "%");
        }
    }
}

// Hash function interface
interface HashFunction {
    Long hash(String key);
}

// MD5 implementation
class MD5HashFunction implements HashFunction {
    @Override
    public Long hash(String key) {
        try {
            java.security.MessageDigest md = 
                java.security.MessageDigest.getInstance("MD5");
            byte[] digest = md.digest(key.getBytes("UTF-8"));
            
            long result = 0;
            for (int i = 0; i < 8; i++) {
                result = (result << 8) | (digest[i] & 0xFF);
            }
            
            return result;
        } catch (Exception e) {
            throw new RuntimeException(e);
        }
    }
}

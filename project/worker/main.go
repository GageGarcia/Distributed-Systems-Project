package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	basePort = 8080
	nextPort = 8081
	portMu   sync.Mutex
	chaosLvl = 0
)

// NodeInstance encapsulates the state of a single isolated virtual node
type NodeInstance struct {
	Port             int
	RecentLogs       []string
	LogMutex         sync.Mutex
	StatusMu         sync.Mutex
	ManualChaosUntil time.Time
	ChaosLevel       int
}

func (n *NodeInstance) recordLog(msg string) {
	n.LogMutex.Lock()
	defer n.LogMutex.Unlock()
	n.RecentLogs = append(n.RecentLogs, msg)
	if len(n.RecentLogs) > 100 {
		n.RecentLogs = n.RecentLogs[1:]
	}
}

// spawnNode initiates an isolated HTTP server process representing a worker node
func spawnNode(port int) {
	node := &NodeInstance{
		Port:       port,
		ChaosLevel: chaosLvl,
		RecentLogs: make([]string, 0),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		node.StatusMu.Lock()
		isManualChaos := time.Now().Before(node.ManualChaosUntil)
		node.StatusMu.Unlock()

		if rand.Intn(100) < node.ChaosLevel || isManualChaos {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("chaos injected: simulating network failure"))
			log.Printf("[Node :%d] Health check failed (chaos mode)", port)
			node.recordLog(fmt.Sprintf("[%s] ERROR: chaos injected: network offline", time.Now().Format(time.RFC3339)))
			return
		}

		node.recordLog(fmt.Sprintf("[%s] INFO: Health check passed", time.Now().Format(time.RFC3339)))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		node.LogMutex.Lock()
		defer node.LogMutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(node.RecentLogs)
	})

	mux.HandleFunc("/chaos", func(w http.ResponseWriter, r *http.Request) {
		node.StatusMu.Lock()
		node.ManualChaosUntil = time.Now().Add(10 * time.Second)
		node.StatusMu.Unlock()

		log.Printf("[Node :%d] Manual chaos fault injected via API!", port)
		node.recordLog(fmt.Sprintf("[%s] WARN: manual chaos fault triggered", time.Now().Format(time.RFC3339)))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fmt.Sprintf("Transaction 200 OK via Port %d", port)))
	})

	// The primary base node acts as the Kubelet proxy pool
	if port == basePort {
		mux.HandleFunc("/spawn", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			portMu.Lock()
			spawnedPort := nextPort
			nextPort++
			portMu.Unlock()

			// Spin up sub-node asynchronously
			go spawnNode(spawnedPort)

			// Resolve host automatically based on exactly who requested this
			hostParts := strings.Split(r.Host, ":")
			hostname := hostParts[0]
			url := fmt.Sprintf("http://%s:%d", hostname, spawnedPort)

			log.Printf("[Kubelet] Provisioned new sub-cluster node at %s", url)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"url": url})
		})
	}

	log.Printf("Worker instance running on :%d", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), mux); err != nil {
		log.Printf("Worker instance on :%d failed: %v", port, err)
	}
}

func main() {
	chaosEnv := os.Getenv("CHAOS_LEVEL")
	if chaosEnv != "" {
		if val, err := strconv.Atoi(chaosEnv); err == nil {
			chaosLvl = val
		}
	}

	portEnv := os.Getenv("PORT")
	if portEnv != "" {
		if val, err := strconv.Atoi(portEnv); err == nil {
			basePort = val
			nextPort = basePort + 1
		}
	}

	log.Printf("🚀 Booting Node Pool Daemon... (Chaos: %d%%)", chaosLvl)
	spawnNode(basePort)
}
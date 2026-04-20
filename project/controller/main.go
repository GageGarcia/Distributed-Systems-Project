package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// CheckResult represents the outcome of an individual health check run
type CheckResult struct {
	Healthy   bool      `json:"healthy"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// HealthCheck is the common interface for all checks (DoorDash 'Run' interface)
type HealthCheck interface {
	Name() string
	Run() CheckResult
}

// HTTPCheck checks endpoint health via HTTP requests
type HTTPCheck struct {
	URL      string
	Endpoint string
	Client   *http.Client
}

func (h *HTTPCheck) Name() string {
	return h.URL + " [HTTP Check]"
}

func (h *HTTPCheck) Run() CheckResult {
	resp, err := h.Client.Get(h.URL + h.Endpoint)
	if err != nil {
		return CheckResult{Healthy: false, Message: err.Error(), Timestamp: time.Now()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return CheckResult{Healthy: true, Message: "HTTP 200 OK", Timestamp: time.Now()}
	}
	return CheckResult{Healthy: false, Message: fmt.Sprintf("HTTP %d", resp.StatusCode), Timestamp: time.Now()}
}

// LogGrepCheck streams logs and scans for error patterns
type LogGrepCheck struct {
	URL          string
	Endpoint     string
	Client       *http.Client
	ErrorPattern string
}

func (l *LogGrepCheck) Name() string {
	return l.URL + " [Log Grep]"
}

func (l *LogGrepCheck) Run() CheckResult {
	resp, err := l.Client.Get(l.URL + l.Endpoint)
	if err != nil {
		return CheckResult{Healthy: false, Message: err.Error(), Timestamp: time.Now()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return CheckResult{Healthy: false, Message: "Could not fetch logs", Timestamp: time.Now()}
	}

	var logs []string
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		return CheckResult{Healthy: false, Message: "Failed to parse logs", Timestamp: time.Now()}
	}

	// Scan for error pattern in the most recent 5 logs
	errorCount := 0
	startIdx := len(logs) - 5
	if startIdx < 0 {
		startIdx = 0
	}
	for i := startIdx; i < len(logs); i++ {
		if strings.Contains(logs[i], l.ErrorPattern) {
			errorCount++
		}
	}

	if errorCount > 0 {
		return CheckResult{Healthy: false, Message: fmt.Sprintf("Found %d occurrences of '%s' recently", errorCount, l.ErrorPattern), Timestamp: time.Now()}
	}

	return CheckResult{Healthy: true, Message: "No critical errors found in recent logs", Timestamp: time.Now()}
}

// ClusterHealthCheck stores the results of checks for a node
type ClusterHealthCheck struct {
	WorkerURL string                 `json:"worker_url"`
	Results   map[string]CheckResult `json:"results"` // Map of check name to result
	History   []bool                 `json:"history"` // Last 5 Overall Status runs to detect flapping
	Status    string                 `json:"status"`  // "Healthy", "Degraded"
	Traffic   uint64                 `json:"traffic"`
}

// ClusterHealthSpec represents the declarative JSON CRD configuration
type ClusterHealthSpec struct {
	Checks []struct {
		Type         string `json:"type"`
		Endpoint     string `json:"endpoint"`
		ErrorPattern string `json:"error_pattern,omitempty"`
	} `json:"checks"`
}

// Alert tracks cluster-wide status changes
type Alert struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Type      string    `json:"type"` // "Healthy", "Critical"
}

// ControllerManager drives the full lifecycle of all health checks
type ControllerManager struct {
	mu      sync.RWMutex
	Nodes   map[string]*ClusterHealthCheck `json:"nodes"`
	Checks  map[string][]HealthCheck       `json:"-"`
	Alerts  []Alert                        `json:"alerts"`
	Overall string                         `json:"cluster_status"` // "Healthy", "Degraded", "Critical"
}

func NewControllerManager(workerURLs []string, specPath string) *ControllerManager {
	manager := &ControllerManager{
		Nodes:   make(map[string]*ClusterHealthCheck),
		Checks:  make(map[string][]HealthCheck),
		Alerts:  make([]Alert, 0),
		Overall: "Pending",
	}

	// Read initial configuration
	for _, url := range workerURLs {
		manager.registerWorker(url, specPath)
	}

	return manager
}

func (c *ControllerManager) registerWorker(url string, specPath string) {
	c.Nodes[url] = &ClusterHealthCheck{
		WorkerURL: url,
		Results:   make(map[string]CheckResult),
		History:   make([]bool, 0),
		Status:    "Pending",
	}
	c.Checks[url] = make([]HealthCheck, 0)

	file, err := os.Open(specPath)
	if err == nil {
		defer file.Close()
		var spec ClusterHealthSpec
		json.NewDecoder(file).Decode(&spec)
		client := &http.Client{Timeout: 2 * time.Second}

		for _, chk := range spec.Checks {
			if chk.Type == "HTTP" {
				c.Checks[url] = append(c.Checks[url], &HTTPCheck{URL: url, Endpoint: chk.Endpoint, Client: client})
			} else if chk.Type == "LogGrep" {
				c.Checks[url] = append(c.Checks[url], &LogGrepCheck{URL: url, Endpoint: chk.Endpoint, Client: client, ErrorPattern: chk.ErrorPattern})
			}
		}
	} else {
		log.Printf("Failed to open spec: %v", err)
	}
}

func (c *ControllerManager) Start() {
	ticker := time.NewTicker(3 * time.Second)
	go func() {
		for {
			<-ticker.C
			c.reconcile()
		}
	}()
	c.reconcile() // immediate first run
}

func (c *ControllerManager) reconcile() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. Validation & Execution: Run all checks CONCURRENTLY
	var wg sync.WaitGroup
	var resultsMu sync.Mutex

	for url, nodeChecks := range c.Checks {
		for _, check := range nodeChecks {
			wg.Add(1)
			go func(u string, chk HealthCheck) {
				defer wg.Done()
				result := chk.Run()

				resultsMu.Lock()
				if node, exists := c.Nodes[u]; exists {
					node.Results[chk.Name()] = result
				}
				resultsMu.Unlock()
			}(url, check)
		}
	}
	wg.Wait()

	// 2. Compute Overall Status for Each Node (Flapping detection)
	healthyNodes := 0
	for _, node := range c.Nodes {
		nodeHealthy := true
		for _, res := range node.Results {
			if !res.Healthy {
				nodeHealthy = false
				break
			}
		}

		node.History = append(node.History, nodeHealthy)
		if len(node.History) > 5 {
			node.History = node.History[1:]
		}

		consecutiveSuccess := 0
		for _, stat := range node.History {
			if stat {
				consecutiveSuccess++
			} else {
				consecutiveSuccess = 0
			}
		}

		if consecutiveSuccess >= 3 {
			node.Status = "Healthy"
			healthyNodes++
		} else {
			node.Status = "Degraded"
		}
	}

	prevOverall := c.Overall
	// 3. Compute Cluster-Wide Status
	if len(c.Nodes) == 0 {
		c.Overall = "Pending"
	} else if healthyNodes == len(c.Nodes) {
		c.Overall = "Healthy"
	} else if healthyNodes == 0 {
		c.Overall = "Critical"
	} else {
		c.Overall = "Degraded"
	}

	if prevOverall != c.Overall && prevOverall != "Pending" {
		alertType := "Critical"
		msg := fmt.Sprintf("Cluster state changed to %s (%d nodes offline)", c.Overall, len(c.Nodes)-healthyNodes)
		if c.Overall == "Healthy" {
			alertType = "Healthy"
			msg = "Cluster completely recovered and is now Healthy"
		}

		c.Alerts = append([]Alert{{Timestamp: time.Now(), Message: msg, Type: alertType}}, c.Alerts...)
		if len(c.Alerts) > 20 {
			c.Alerts = c.Alerts[:20]
		}
	}

	log.Printf("[ControllerManager] Status: %s | %d/%d instances healthy", c.Overall, healthyNodes, len(c.Nodes))
}

func main() {
	log.Println("🚀 Initializing DoorDash-style Cluster Health Framework...")

	workerEnv := os.Getenv("WORKERS")
	workerURLs := []string{}
	if workerEnv != "" {
		workerURLs = strings.Split(workerEnv, ",")
	}

	manager := NewControllerManager(workerURLs, "cluster-health-spec.json")
	manager.Start()

	// Emitting state for observability
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(manager)
	})

	// Auto-Scaling API
	http.HandleFunc("/auto-scale", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Dial worker-1's Node Pool API
		resp, err := http.Post("http://worker-1:8080/spawn", "application/json", nil)
		if err != nil {
			http.Error(w, fmt.Sprintf("Kubelet connection failed: %v", err), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		var data map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			http.Error(w, "Invalid Kubelet response", http.StatusInternalServerError)
			return
		}

		newURL := data["url"]

		manager.mu.Lock()
		if _, exists := manager.Nodes[newURL]; !exists {
			manager.registerWorker(newURL, "cluster-health-spec.json")
			log.Printf("[Auto-Scaler] Provisioned sub-node: %s", newURL)
		}
		manager.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": newURL})
	})
	
	// Intelligent Load Balancer Reverse Proxy
	http.HandleFunc("/proxy/work", func(w http.ResponseWriter, r *http.Request) {
		manager.mu.RLock()
		var healthyNodes []string
		for url, node := range manager.Nodes {
			if node.Status == "Healthy" {
				healthyNodes = append(healthyNodes, url)
			}
		}
		manager.mu.RUnlock()

		if len(healthyNodes) == 0 {
			http.Error(w, "503 Service Unavailable: No healthy pool instances available", http.StatusServiceUnavailable)
			return
		}

		targetURL := healthyNodes[rand.Intn(len(healthyNodes))]

		resp, err := http.Get(targetURL + "/work")
		if err != nil {
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			manager.mu.Lock()
			if node, exists := manager.Nodes[targetURL]; exists {
				node.Traffic++
			}
			manager.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Proxied to " + targetURL))
	})

	// Dynamic Registry API
	http.HandleFunc("/workers", func(w http.ResponseWriter, r *http.Request) {
		manager.mu.Lock()
		defer manager.mu.Unlock()

		nodeURL := r.URL.Query().Get("url")
		if nodeURL == "" {
			http.Error(w, "missing param url", http.StatusBadRequest)
			return
		}

		if r.Method == "POST" {
			if _, exists := manager.Nodes[nodeURL]; !exists {
				manager.registerWorker(nodeURL, "cluster-health-spec.json")
				log.Printf("Registered new worker node: %s", nodeURL)
			}
			w.WriteHeader(http.StatusOK)
		} else if r.Method == "DELETE" {
			delete(manager.Nodes, nodeURL)
			delete(manager.Checks, nodeURL)
			log.Printf("Decommissioned worker node: %s", nodeURL)
			w.WriteHeader(http.StatusOK)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Expose Prometheus Metrics Endpoint
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		
		var b strings.Builder
		b.WriteString("# HELP cluster_health_status Overall health of the cluster (1=Healthy, 0=Degraded/Critical)\n")
		b.WriteString("# TYPE cluster_health_status gauge\n")
		val := 0
		if manager.Overall == "Healthy" { val = 1 }
		b.WriteString(fmt.Sprintf("cluster_health_status %d\n", val))
		
		b.WriteString("# HELP node_health_status Individual node health status\n")
		b.WriteString("# TYPE node_health_status gauge\n")
		for url, node := range manager.Nodes {
			nVal := 0
			if node.Status == "Healthy" { nVal = 1 }
			b.WriteString(fmt.Sprintf("node_health_status{node=\"%s\"} %d\n", url, nVal))
		}
		
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte(b.String()))
	})

	// Proxy Chaos Fault
	http.HandleFunc("/inject-fault", func(w http.ResponseWriter, r *http.Request) {
		nodeURL := r.URL.Query().Get("node")
		if nodeURL == "" {
			http.Error(w, "missing node parameter", http.StatusBadRequest)
			return
		}

		go http.Post(nodeURL+"/chaos", "application/json", nil)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Fault injected"))
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "dashboard.html")
			return
		}
		http.NotFound(w, r)
	})

	log.Println("Controller started on :9090")
	log.Fatal(http.ListenAndServe(":9090", nil))
}

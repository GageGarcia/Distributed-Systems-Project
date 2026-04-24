# DoorDash Cluster Health Framework

A practical, microservice-based distributed system built in Go that implements proactive cluster observability, auto-scaling, and "chaos" fault tolerance.

## Architecture

This project is built using an active-active cluster design orchestrated via **Docker Compose**:
- **Controller Manager**: A centralized intelligent load-balancer and health monitor.
- **Worker Nodes**: Isolated Go-based HTTP servers processing asynchronous requests.

Unlike strictly theoretical distributed frameworks (like Raft or 2PC implementations), this system runs across an actual local container network to demonstrate fault-tolerance under real networking conditions.

## Key Features

1. **Intelligent Reverse Proxy**: The controller safely routes end-user traffic by continuously filtering against an active routing table of known healthy nodes. 
2. **Declarative Health Probes**:
   - `HTTP Checking`: Verifies immediate responsiveness.
   - `Log Grepping`: Parses telemetry to catch hidden backend failures that aren't throwing HTTP errors.
3. **Flapping Detection**: A node must pass 3 consecutive state checks to be upgraded to `Healthy`. This prevents unstable nodes from repeatedly bouncing in and out of the active pool, maintaining a smooth user experience.
4. **Chaos Engineering**: Built-in environment flags (e.g., `CHAOS_LEVEL=40`) intentionally introduce non-deterministic packet dropping and connection failures into specific worker containers to prove systemic fault tolerance.
5. **Dynamic Auto-Scaling (Kubelet)**: Features an internal `/spawn` API that allows the primary node to act as a Kubelet daemon, instantly spinning up and registering isolated sub-processes to handle high traffic demands.
6. **Local Observability Dashboard**: A visualization UI that tracks live traffic delegation and cluster health state in real-time.

## Getting Started

Because the system is fully containerized, booting the cluster requires Docker.

Make sure the Docker daemon is running, then simply boot the system:
```bash
docker-compose up --build
```
*You can gracefully shut down the cluster and clean up the containers using `docker-compose down`.*

### Viewing the Dashboard
Once the system has successfully booted, navigate to:
```
http://localhost:9090/
```
From here, you can watch the Controller route traffic, manually inject faults to specific nodes, and monitor the cluster's recovery process.

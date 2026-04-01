# GKE ORCA Metrics Sidecar

This repository provides a lightweight, Go-based reverse proxy sidecar designed to run as a **native Kubernetes sidecar**. Its primary purpose is to enable advanced **Weighted Round Robin (WRR)** traffic distribution via Google Cloud Load Balancing (GCLB) by dynamically reporting application utilization using the **Open Request Cost Aggregation (ORCA)** standard.

By proxying traffic to your main application container and utilizing the Kubernetes shared process namespace, this sidecar actively monitors the application's CPU and memory footprint, aggregates the data across multi-process applications (like Apache/PHP workers), and injects real-time load metrics into the HTTP response headers as JSON.

---

## How It Works

The sidecar operates entirely in the background of your application pod without requiring any changes to your core application code.

- **Process Inspection:** It uses the shared K8s process namespace to read `/proc` metrics for a specifically named application process.
- **Aggregation:** It identifies all child and worker processes sharing that name and aggregates their total utilization.
- **Reverse Proxying:** It intercepts incoming HTTP requests on its designated port, forwards them to the application container, and waits for the response.
- **Header Injection:** Upon receiving the response from the application, the sidecar computes the latest utilization metrics and injects them into the `Endpoint-Load-Metrics` and `X-Endpoint-Load-Metrics` headers as standardized ORCA JSON payloads.
- **Traffic Routing:** GCLB reads these headers natively and adjusts its routing decisions, shifting traffic away from heavily loaded pods toward pods with spare capacity.

---

## Configuration Variables

The sidecar is configured entirely via environment variables.

| Variable Name | Required | Default Value | Description |
| :--- | :--- | :--- | :--- |
| **APP_PROCESS_NAME** | Yes | None | The exact name of the executable process the sidecar should monitor (e.g., `apache2`, `node`, `java`). |
| **APP_PORT** | No | `8080` | The port your primary application container is listening on. The sidecar forwards traffic here. |
| **SIDECAR_PORT** | No | `9090` | The port the sidecar proxy will bind to and listen for incoming external traffic. |
| **METRICS_INTERVAL** | No | `1s` | How frequently the sidecar polls the OS for updated CPU and Memory metrics (e.g., `500ms`, `2s`). |

> **Pro-Tip: Port Matching**
> Ensure that your K8s Service targets the `SIDECAR_PORT` rather than the application port. Additionally, the `SIDECAR_PORT` environment variable **must** identically match the `containerPort` declared for the sidecar in your K8s manifest so that traffic routes correctly.

---

## Kubernetes Deployment Setup

This project leverages the modern Kubernetes 1.28+ native sidecar pattern by utilizing the `initContainers` block with `restartPolicy: Always`. 

Crucially, you must enable `shareProcessNamespace: true` so the sidecar can "see" the application's running processes.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gclb-orca-demo
  labels:
    app: backend-service
spec:
  replicas: 3
  selector:
    matchLabels:
      app: backend-service
  template:
    metadata:
      labels:
        app: backend-service
    spec:
      # CRITICAL: Enables cross-container process inspection
      shareProcessNamespace: true
      
      initContainers:
      - name: orca-proxy-sidecar
        image: your-registry/orca-sidecar:latest
        # CRITICAL: Defines this as a Native Sidecar (K8s 1.28+)
        restartPolicy: Always
        env:
        - name: APP_PORT
          value: "8080"
        - name: SIDECAR_PORT
          value: "9090"
        - name: APP_PROCESS_NAME
          value: "apache2"
        - name: METRICS_INTERVAL
          value: "2s"
        ports:
        - name: proxy-http
          containerPort: 9090
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
            
      containers:
      - name: main-application
        image: registry.k8s.io/hpa-example:latest
        ports:
        - name: app-http
          containerPort: 8080
        resources:
          requests:
            cpu: 500m
            memory: 256Mi
```
## GKE Traffic Distribution Policy
To actually instruct Google Cloud Load Balancing to perform Weighted Round Robin (WRR) based on these injected metrics, you must attach a GCPTrafficDistributionPolicy to your target Kubernetes Service.

> **Important Data-Plane vs Control-Plane Casing**
> While the sidecar injects the header payload using camelCase JSON ({"cpuUtilization": 0.45}), the GKE API strictly requires snake_case with an explicit orca. namespace prefix (orca.cpu_utilization) to safely route the configuration to the underlying load balancer.

```yaml
apiVersion: networking.gke.io/v1
kind: GCPTrafficDistributionPolicy
metadata:
  name: orca-cpu-wrr-policy
  namespace: default
spec:
  targetRefs:
  - kind: Service
    group: ""
    # Make sure this matches the Service targeting your deployment
    name: backend-service 
  default:
    # Enables dynamic metric-based endpoint weighting
    localityLbAlgorithm: WEIGHTED_ROUND_ROBIN
    customMetrics:
    # The strictly required format for GKE Gateway API
    - name: orca.cpu_utilization
      dryRun: false
```
Once applied, the GKE Gateway Controller will automatically configure your backend services to ingest the JSON headers and balance external traffic intelligently based on the real-time aggregated CPU usage of your application processes.

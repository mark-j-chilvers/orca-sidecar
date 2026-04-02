package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	orcapb "github.com/cncf/xds/go/xds/data/orca/v3"
	"github.com/shirou/gopsutil/v3/process"
	"google.golang.org/protobuf/encoding/protojson"
)

var (
	currentCPU float64
	currentMem float64
	currentRPS float64
	currentEPS float64
	mu         sync.RWMutex

	// Atomic counters for traffic tracking
	totalRequests uint64
	totalErrors   uint64
)

// collectMetrics runs in the background to periodically poll OS metrics
// and calculate traffic rates (RPS and EPS).
func collectMetrics(appProcessName string, interval time.Duration) {
	var lastReqCount uint64
	var lastErrCount uint64
	lastTime := time.Now()

	for {
		var totalCPU float64
		var totalMem float32
		var cpuCount int32
		var memCount int32
		var found bool

		// 1. Collect OS Metrics (Your existing logic)
		procs, err := process.Processes()
		if err == nil {
			for _, p := range procs {
				name, _ := p.Name()
				if name == appProcessName {
					found = true

					cpuPct, err := p.CPUPercent()
					if err == nil {
						totalCPU += cpuPct
						if cpuPct > 0 {
							cpuCount++
						}
					}

					memPct, err := p.MemoryPercent()
					if err == nil {
						totalMem += memPct
						if memPct > 0 {
							memCount++
						}
					}
				}
			}
		}

		// 2. Calculate Traffic Rates (RPS & EPS)
		now := time.Now()
		elapsed := now.Sub(lastTime).Seconds()

		// Safely load the current atomic counts
		reqs := atomic.LoadUint64(&totalRequests)
		errs := atomic.LoadUint64(&totalErrors)

		// Calculate rate per second
		rps := float64(reqs-lastReqCount) / elapsed
		eps := float64(errs-lastErrCount) / elapsed

		// Update tracking variables for the next loop
		lastReqCount = reqs
		lastErrCount = errs
		lastTime = now

		// 3. Save all metrics to the global state
		mu.Lock()
		if found {
			if cpuCount == 0 {
				cpuCount = 1
			}
			if memCount == 0 {
				memCount = 1
			}
			currentCPU = totalCPU / float64(cpuCount)
			currentMem = float64(totalMem) / float64(memCount)
		}
		currentRPS = rps
		currentEPS = eps
		mu.Unlock()

		if !found {
			log.Printf("Waiting for processes named: %s", appProcessName)
		}

		time.Sleep(interval)
	}
}

func main() {
	appPort := os.Getenv("APP_PORT")
	if appPort == "" {
		appPort = "8080"
	}

	sidecarPort := os.Getenv("SIDECAR_PORT")
	if sidecarPort == "" {
		sidecarPort = "9090"
	}

	appProcessName := os.Getenv("APP_PROCESS_NAME")
	if appProcessName == "" {
		log.Fatal("APP_PROCESS_NAME environment variable must be set")
	}

	intervalStr := os.Getenv("METRICS_INTERVAL")
	pollInterval := 1 * time.Second
	if intervalStr != "" {
		parsedDuration, err := time.ParseDuration(intervalStr)
		if err != nil {
			log.Printf("Invalid METRICS_INTERVAL format '%s', falling back to 1s: %v", intervalStr, err)
		} else {
			pollInterval = parsedDuration
		}
	}

	log.Printf("Starting metric collection for '%s' every %v", appProcessName, pollInterval)
	go collectMetrics(appProcessName, pollInterval)

	targetURL, err := url.Parse(fmt.Sprintf("http://localhost:%s", appPort))
	if err != nil {
		log.Fatalf("Invalid app port: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// NEW: Handle network-level errors (timeouts, connection refused)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("Proxy backend error: %v", err)

		// 1. A request was still attempted, so increment total requests
		atomic.AddUint64(&totalRequests, 1)
		
		// 2. The backend failed to respond, so increment total errors
		atomic.AddUint64(&totalErrors, 1)

		// 3. Return a standard 502 Bad Gateway to the load balancer
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintln(w, "Bad Gateway")
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		// 1. Track the request and potential errors
		atomic.AddUint64(&totalRequests, 1)

		// Typically, 5xx status codes indicate application/server errors
		if resp.StatusCode >= 500 {
			atomic.AddUint64(&totalErrors, 1)
		}

		// 2. Read current metrics
		mu.RLock()
		cpu := currentCPU
		mem := currentMem
		rps := currentRPS
		eps := currentEPS
		mu.RUnlock()

		// 3. Build the payload
		loadReport := &orcapb.OrcaLoadReport{
			CpuUtilization: cpu,
			MemUtilization: mem,
			RpsFractional:  rps,
			Eps:            eps,
		}

		// Configure protojson to output snake_case keys AND include zero values
		marshalOpts := protojson.MarshalOptions{
			UseProtoNames:   true,
			EmitUnpopulated: true,
		}

		jsonBytes, err := marshalOpts.Marshal(loadReport)

		if err == nil {
			jsonString := string(jsonBytes)

			resp.Header.Set("Endpoint-Load-Metrics", "JSON "+jsonString)
			resp.Header.Set("X-Endpoint-Load-Metrics", "JSON "+jsonString)
		} else {
			log.Printf("Failed to marshal ORCA protobuf to JSON: %v", err)
		}

		return nil
	}

	log.Printf("Starting ORCA sidecar proxy on port %s -> %s", sidecarPort, appPort)
	if err := http.ListenAndServe(fmt.Sprintf(":%s", sidecarPort), proxy); err != nil {
		log.Fatalf("Sidecar proxy failed: %v", err)
	}
}

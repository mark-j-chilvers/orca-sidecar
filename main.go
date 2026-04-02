package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"

	orcapb "github.com/cncf/xds/go/xds/data/orca/v3"
	"github.com/shirou/gopsutil/v3/process"
)

var (
	currentCPU float64
	currentMem float64
	mu         sync.RWMutex
)

// collectMetrics runs in the background to periodically poll the target processes
// for their aggregated CPU and Memory utilization.
func collectMetrics(appProcessName string, interval time.Duration) {
	for {
		var totalCPU float64
		var totalMem float32
		var cpuCount int32
		var memCount int32

		var found bool

		procs, err := process.Processes()
		if err == nil {
			for _, p := range procs {
				name, _ := p.Name()
				// Match every process that shares the target name (e.g., all apache2 workers)
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

		if found {
			mu.Lock()
			//avoid a poss div by zero
			if cpuCount == 0 {
				cpuCount = 1
			}
			if memCount == 0 {
				memCount = 1
			}
			currentCPU = totalCPU / float64(cpuCount)
			currentMem = float64(totalMem) / float64(memCount)
			mu.Unlock()
		} else {
			log.Printf("Waiting for processes named: %s", appProcessName)
		}

		// Sleep for the configured interval before polling again
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

	// 1. Parse the configurable interval
	intervalStr := os.Getenv("METRICS_INTERVAL")
	pollInterval := 1 * time.Second // Default to 1 second
	if intervalStr != "" {
		parsedDuration, err := time.ParseDuration(intervalStr)
		if err != nil {
			log.Printf("Invalid METRICS_INTERVAL format '%s', falling back to 1s: %v", intervalStr, err)
		} else {
			pollInterval = parsedDuration
		}
	}

	// Start background metric collection with the configured interval
	log.Printf("Starting metric collection for '%s' every %v", appProcessName, pollInterval)
	go collectMetrics(appProcessName, pollInterval)

	targetURL, err := url.Parse(fmt.Sprintf("http://localhost:%s", appPort))
	if err != nil {
		log.Fatalf("Invalid app port: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	proxy.ModifyResponse = func(resp *http.Response) error {
		mu.RLock()
		cpu := currentCPU
		mem := currentMem
		mu.RUnlock()

		loadReport := &orcapb.OrcaLoadReport{
			CpuUtilization: cpu,
			MemUtilization: mem,
		}

		// 2. Marshal the Protobuf to JSON instead of Binary/Base64
		jsonBytes, err := json.Marshal(loadReport)

		if err == nil {
			jsonString := string(jsonBytes)

			// Note: The header name drops the "-Bin" suffix when sending JSON or text
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

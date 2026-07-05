package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ── gost /config API response structures ───────────────────────────────

type ConfigResponse struct {
	Services []Service `json:"services"`
	Chains   []Chain   `json:"chains,omitempty"`
}

type Service struct {
	Name    string      `json:"name"`
	Addr    string      `json:"addr"`
	Status  Status      `json:"status"`
	Handler interface{} `json:"handler,omitempty"`
}

type Status struct {
	CreateTime int64   `json:"createTime"`
	State      string  `json:"state"`
	Events     []Event `json:"events,omitempty"`
}

type Event struct {
	Time int64  `json:"time"`
	Msg  string `json:"msg"`
}

type Chain struct {
	Name string `json:"name"`
	Hops []Hop  `json:"hops,omitempty"`
}

type Hop struct {
	Name  string `json:"name"`
	Nodes []Node `json:"nodes,omitempty"`
}

type Node struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// ── Prometheus metrics ──────────────────────────────────────────────────

var (
	gostScrapeErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gost_scrape_errors_total",
		Help: "Total number of errors scraping gost API",
	})

	gostUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gost_up",
		Help: "Whether the gost API is reachable (1=up, 0=down)",
	})

	gostServiceReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gost_service_ready",
		Help: "Whether a gost service is in ready state (1=ready, 0=not)",
	}, []string{"name", "addr"})

	gostServiceCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gost_services_total",
		Help: "Total number of gost services configured",
	})

	gostChainCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gost_chains_total",
		Help: "Total number of gost chains configured",
	})
)

// ── Exporter ────────────────────────────────────────────────────────────

type Exporter struct {
	gostURL string
	client  *http.Client
	mu      sync.Mutex
}

func NewExporter(gostURL string) *Exporter {
	return &Exporter{
		gostURL: gostURL,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (e *Exporter) scrape() {
	e.mu.Lock()
	defer e.mu.Unlock()

	url := e.gostURL + "/config"
	resp, err := e.client.Get(url)
	if err != nil {
		log.Printf("ERROR: failed to scrape %s: %v", url, err)
		gostUp.Set(0)
		gostScrapeErrors.Inc()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("ERROR: %s returned status %d", url, resp.StatusCode)
		gostUp.Set(0)
		gostScrapeErrors.Inc()
		return
	}

	var data ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("ERROR: failed to decode config response: %v", err)
		gostUp.Set(0)
		gostScrapeErrors.Inc()
		return
	}

	gostUp.Set(1)
	gostServiceReady.Reset()

	gostServiceCount.Set(float64(len(data.Services)))
	gostChainCount.Set(float64(len(data.Chains)))

	for _, svc := range data.Services {
		ready := 0.0
		if svc.Status.State == "ready" {
			ready = 1.0
		}
		gostServiceReady.WithLabelValues(svc.Name, svc.Addr).Set(ready)
	}

	log.Printf("Scraped gost config: %d services, %d chains", len(data.Services), len(data.Chains))
}

// ── Main ────────────────────────────────────────────────────────────────

func main() {
	gostURL := getEnv("GOST_URL", "http://tunnelium-gost-incoming:18080")
	listenAddr := getEnv("LISTEN_ADDR", ":9130")

	log.Printf("gost exporter starting")
	log.Printf("  gost API: %s", gostURL)
	log.Printf("  listen:   %s", listenAddr)

	prometheus.MustRegister(gostScrapeErrors, gostUp, gostServiceReady, gostServiceCount, gostChainCount)

	exporter := NewExporter(gostURL)
	exporter.scrape()

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			exporter.scrape()
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	log.Printf("listening on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("FATAL: %v", err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

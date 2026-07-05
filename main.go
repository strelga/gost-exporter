package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ── Prometheus metrics ──────────────────────────────────────────────────

var (
	gostScrapeErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gost_scrape_errors_total",
		Help: "Total number of errors scraping gost debug API",
	})

	gostUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gost_up",
		Help: "Whether the gost debug API is reachable (1=up, 0=down)",
	})

	// Dynamic gauges for all expvar values from gost.
	// The "key" label holds the flattened expvar key name.
	gostVarGauges = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gost_var",
		Help: "Dynamic gauge for gost expvar metrics",
	}, []string{"key"})
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
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (e *Exporter) scrape() {
	e.mu.Lock()
	defer e.mu.Unlock()

	url := e.gostURL + "/debug/vars"
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

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("ERROR: failed to decode expvar response: %v", err)
		gostUp.Set(0)
		gostScrapeErrors.Inc()
		return
	}

	gostUp.Set(1)

	// Reset dynamic gauges and repopulate
	gostVarGauges.Reset()

	count := 0
	for key, value := range data {
		// Skip standard Go expvar keys
		if key == "cmdline" || key == "memstats" {
			continue
		}
		count += flattenAndCollect("", key, value)
	}

	log.Printf("Scraped gost expvar: %d metrics collected", count)
}

// flattenAndCollect recursively flattens nested JSON into Prometheus gauge values.
// Example: {"service": {"socks5": {"handler": {"conn_total": 42}}}}
//
//	→ label key="service_socks5_handler_conn_total" value=42
func flattenAndCollect(prefix, key string, value interface{}) int {
	fullKey := key
	if prefix != "" {
		fullKey = prefix + "_" + key
	}

	switch v := value.(type) {
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			gostVarGauges.WithLabelValues(sanitizeKey(fullKey)).Set(v)
			return 1
		}
	case json.Number:
		if f, err := v.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			gostVarGauges.WithLabelValues(sanitizeKey(fullKey)).Set(f)
			return 1
		}
	case map[string]interface{}:
		count := 0
		for k, val := range v {
			count += flattenAndCollect(fullKey, k, val)
		}
		return count
	}
	return 0
}

// sanitizeKey converts a dotted/dashed key into a valid Prometheus label value.
func sanitizeKey(s string) string {
	r := strings.NewReplacer(".", "_", "-", "_", " ", "_", "/", "_")
	s = r.Replace(s)
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	return strings.Trim(s, "_")
}

// ── Main ────────────────────────────────────────────────────────────────

func main() {
	gostURL := getEnv("GOST_URL", "http://tunnelium-gost-incoming:18080")
	listenAddr := getEnv("LISTEN_ADDR", ":9130")
	scrapeInterval := 15 * time.Second

	log.Printf("gost exporter starting")
	log.Printf("  gost API:  %s", gostURL)
	log.Printf("  listen:    %s", listenAddr)
	log.Printf("  interval:  %s", scrapeInterval)

	prometheus.MustRegister(gostScrapeErrors, gostUp, gostVarGauges)

	exporter := NewExporter(gostURL)
	exporter.scrape()

	go func() {
		ticker := time.NewTicker(scrapeInterval)
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

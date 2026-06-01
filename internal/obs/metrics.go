package obs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// metricPoint holds a parsed data point from the metrics NDJSON file.
type metricPoint struct {
	name  string
	value float64
	attrs map[string]string
	ts    string
}

// FormatMetrics reads the metrics.jsonl file and returns a human-readable
// summary of the most recent value for each metric+attribute combination.
func FormatMetrics(otelDir string) string {
	path := filepath.Join(otelDir, "metrics.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "no metrics recorded yet (run a few turns first)"
		}
		return fmt.Sprintf("error reading metrics: %v", err)
	}
	defer f.Close()

	// latest maps "name{labels}" → metricPoint, keeping only the most recent entry.
	latest := map[string]metricPoint{}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		parseMetricLine(line, latest)
	}

	if len(latest) == 0 {
		return "metrics file exists but contains no recognisable data points yet"
	}

	// Sort keys for stable output.
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintln(&b, "metrics (most recent values):")
	for _, k := range keys {
		p := latest[k]
		label := p.name
		if len(p.attrs) > 0 {
			parts := make([]string, 0, len(p.attrs))
			for ak, av := range p.attrs {
				parts = append(parts, ak+"="+av)
			}
			sort.Strings(parts)
			label += "{" + strings.Join(parts, ",") + "}"
		}
		fmt.Fprintf(&b, "  %-60s %g\n", label, p.value)
	}
	if b.Len() > 0 {
		fmt.Fprint(&b, "hint: /otel for file sizes, /otel trim to archive")
	}
	return strings.TrimRight(b.String(), "\n")
}

// parseMetricLine extracts data points from one OTLP JSON metrics line.
// The stdoutmetric exporter writes one JSON object per export cycle.
func parseMetricLine(line string, out map[string]metricPoint) {
	var root struct {
		ScopeMetrics []struct {
			Metrics []struct {
				Name string `json:"Name"`
				Data struct {
					// Sum / Gauge share DataPoints
					DataPoints []struct {
						Attributes []struct {
							Key   string `json:"Key"`
							Value struct {
								Value any `json:"Value"`
							} `json:"Value"`
						} `json:"Attributes"`
						StartTime string  `json:"StartTime"`
						Time      string  `json:"Time"`
						Value     float64 `json:"Value"`
						// Int gauge uses Value too, but as int in some versions
					} `json:"DataPoints"`
				} `json:"Data"`
			} `json:"Metrics"`
		} `json:"ScopeMetrics"`
	}
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return
	}
	for _, sm := range root.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, dp := range m.Data.DataPoints {
				attrs := map[string]string{}
				for _, a := range dp.Attributes {
					attrs[a.Key] = fmt.Sprintf("%v", a.Value.Value)
				}
				key := metricKey(m.Name, attrs)
				out[key] = metricPoint{
					name:  m.Name,
					value: dp.Value,
					attrs: attrs,
					ts:    dp.Time,
				}
			}
		}
	}
}

func metricKey(name string, attrs map[string]string) string {
	if len(attrs) == 0 {
		return name
	}
	parts := make([]string, 0, len(attrs))
	for k, v := range attrs {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return name + "{" + strings.Join(parts, ",") + "}"
}

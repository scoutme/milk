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
	// instance is the record's Resource service.instance.id ("" for records
	// written before instance attribution existed, #455).
	instance string
	// cumulative is true when the instrument exported CumulativeTemporality
	// (a counter): cumulative values from different processes are additive,
	// gauges/last-values are not.
	cumulative bool
}

// FormatMetrics reads the metrics.jsonl file and returns a human-readable
// summary of the most recent value for each metric+attribute combination.
//
// Several milk sessions append to this file at once (#456), so "most recent"
// is resolved per writer instance and then aggregated instead of letting the
// last flusher win: cumulative counters are summed across instances (each
// process's counter covers its own lifetime — the sum is the true total
// since the last trim), while gauges and other last-value instruments take
// the datapoint with the newest timestamp (N sessions observe the same file
// or store; summing those would multiply them).
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

	// metric label → instance → latest point for that pair.
	latest := map[string]map[string]metricPoint{}

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

	type rendered struct {
		value float64
		ts    string
	}
	agg := map[string]rendered{}
	for label, byInst := range latest {
		instances := make([]string, 0, len(byInst))
		for inst := range byInst {
			instances = append(instances, inst)
		}
		sort.Strings(instances) // deterministic tie-breaks

		cumulative := byInst[instances[0]].cumulative
		var r rendered
		if cumulative {
			for _, inst := range instances {
				p := byInst[inst]
				r.value += p.value
				if p.ts > r.ts {
					r.ts = p.ts
				}
			}
		} else {
			for _, inst := range instances {
				p := byInst[inst]
				if r.ts == "" || p.ts >= r.ts {
					r = rendered{value: p.value, ts: p.ts}
				}
			}
		}
		agg[label] = r
	}

	keys := make([]string, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintln(&b, "metrics (most recent values):")
	for _, k := range keys {
		r := agg[k]
		fmt.Fprintf(&b, "  %-60s %g", k, r.value)
		if r.ts != "" {
			fmt.Fprintf(&b, "  @ %s", r.ts)
		}
		fmt.Fprintln(&b)
	}
	if b.Len() > 0 {
		fmt.Fprint(&b, "hint: /otel for file sizes, /otel trim to archive")
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseMetricLine(line string, out map[string]map[string]metricPoint) {
	var root struct {
		Resource []struct {
			Key   string `json:"Key"`
			Value struct {
				Value any `json:"Value"`
			} `json:"Value"`
		} `json:"Resource"`
		ScopeMetrics []struct {
			Metrics []struct {
				Name string `json:"Name"`
				Data struct {
					DataPoints []struct {
						Attributes []struct {
							Key   string `json:"Key"`
							Value struct {
								Value any `json:"Value"`
							} `json:"Value"`
						} `json:"Attributes"`
						Time  string  `json:"Time"`
						Value float64 `json:"Value"`
					} `json:"DataPoints"`
					Temporality string `json:"Temporality"`
				} `json:"Data"`
			} `json:"Metrics"`
		} `json:"ScopeMetrics"`
	}
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return
	}
	instance := ""
	for _, a := range root.Resource {
		if a.Key == "service.instance.id" {
			instance = fmt.Sprintf("%v", a.Value.Value)
		}
	}
	for _, sm := range root.ScopeMetrics {
		for _, m := range sm.Metrics {
			cumulative := m.Data.Temporality == "CumulativeTemporality"
			for _, dp := range m.Data.DataPoints {
				attrs := map[string]string{}
				for _, a := range dp.Attributes {
					attrs[a.Key] = fmt.Sprintf("%v", a.Value.Value)
				}
				label := metricKey(m.Name, attrs)
				byInst, ok := out[label]
				if !ok {
					byInst = map[string]metricPoint{}
					out[label] = byInst
				}
				byInst[instance] = metricPoint{
					name: m.Name, value: dp.Value, attrs: attrs,
					ts: dp.Time, instance: instance, cumulative: cumulative,
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

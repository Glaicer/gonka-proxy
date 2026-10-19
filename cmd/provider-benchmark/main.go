package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/glaicer/gonka-proxy/internal/benchmark"
	"github.com/glaicer/gonka-proxy/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to proxy YAML configuration")
	outputPath := flag.String("output", "-", "output path, or - for stdout")
	format := flag.String("format", "json", "output format: json or csv")
	rounds := flag.Int("rounds", 3, "sequential requests per provider model")
	maxTokens := flag.Int("max-tokens", 8, "maximum output tokens per request")
	timeout := flag.Duration("timeout", 45*time.Second, "per-request timeout")
	delay := flag.Duration("delay", 500*time.Millisecond, "delay between requests")
	providerFilter := flag.String("provider", "", "comma-separated provider names to test; default all")
	modelFilter := flag.String("model", "", "comma-separated model names to test; default all")
	match := flag.String("match", "", "case-insensitive substring the model name must contain")
	discoverOnly := flag.Bool("discover-only", false, "resolve model lists without sending chat requests")
	flag.Parse()

	if *format != "json" && *format != "csv" {
		fatalf("format must be json or csv")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatalf("load config: %v", err)
	}
	unknown := unknownProviders(cfg, splitList(*providerFilter))
	if len(unknown) > 0 {
		fatalf("unknown provider(s): %s", strings.Join(unknown, ", "))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := benchmark.Run(ctx, cfg, benchmark.Options{
		Rounds:       *rounds,
		MaxTokens:    *maxTokens,
		Timeout:      *timeout,
		Delay:        *delay,
		Providers:    splitList(*providerFilter),
		Models:       splitList(*modelFilter),
		Match:        *match,
		DiscoverOnly: *discoverOnly,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fatalf("run benchmark: %v", err)
	}
	if err := writeReport(os.Stdout, *outputPath, *format, report); err != nil {
		fatalf("write report: %v", err)
	}
}

func writeReport(stdout io.Writer, path, format string, report benchmark.Report) error {
	var output io.Writer = stdout
	var file *os.File
	if path != "-" {
		var err error
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		output = file
	}
	if format == "csv" {
		return writeCSV(output, report)
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeCSV(output io.Writer, report benchmark.Report) error {
	writer := csv.NewWriter(output)
	if err := writer.Write([]string{
		"generated_at", "provider", "model_source", "model", "round", "started_at", "status",
		"error_category", "status_code", "header_latency_ms", "first_byte_latency_ms",
		"total_latency_ms", "response_bytes",
	}); err != nil {
		return err
	}
	for _, provider := range report.Results {
		for _, run := range provider.Runs {
			if err := writer.Write([]string{
				report.GeneratedAt.Format(time.RFC3339Nano), provider.Provider, provider.ModelSource, run.Model,
				strconv.Itoa(run.Round), run.StartedAt.Format(time.RFC3339Nano), run.Status,
				run.ErrorCategory, strconv.Itoa(run.StatusCode), strconv.FormatInt(run.HeaderLatencyMS, 10),
				strconv.FormatInt(run.FirstByteLatencyMS, 10), strconv.FormatInt(run.TotalLatencyMS, 10),
				strconv.FormatInt(run.ResponseBytes, 10),
			}); err != nil {
				return err
			}
		}
	}
	writer.Flush()
	return writer.Error()
}

// splitList parses a comma-separated CLI list into trimmed non-empty entries.
func splitList(value string) []string {
	items := make([]string, 0, strings.Count(value, ",")+1)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// unknownProviders returns requested provider names missing from the config.
func unknownProviders(cfg config.Config, requested []string) []string {
	if len(requested) == 0 {
		return nil
	}
	configured := make(map[string]struct{}, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		configured[provider.Name] = struct{}{}
	}
	var unknown []string
	for _, name := range requested {
		if _, ok := configured[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

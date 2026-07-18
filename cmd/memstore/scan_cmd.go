package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
	"github.com/matthewjhunter/memstore/internal/fence"
	"github.com/matthewjhunter/memstore/internal/screening"
)

// buildScanGenerator returns the chat generator the scan should screen with, preferring
// the daemon (which already owns model config and credentials) and falling back to a
// directly configured OpenAI-compatible endpoint.
func buildScanGenerator() (screening.Generator, error) {
	if cliConfig.Remote != "" {
		return httpclient.NewHTTPGenerator(cliConfig.Remote, cliConfig.APIKey), nil
	}
	if cliConfig.Ollama == "" || cliConfig.GenModel == "" {
		return nil, fmt.Errorf("no remote and no local chat model configured (set MEMSTORE_REMOTE, or OLLAMA/GEN_MODEL)")
	}
	return memstore.NewOpenAIGenerator(cliConfig.Ollama, cliConfig.LLMAPIKey, cliConfig.GenModel), nil
}

// runScan screens the existing corpus and reports what enforcement would have done to
// it, without changing anything.
//
// This exists because the screening thresholds are uncalibrated. airlock does not
// claim its threat scale is calibrated, memstore's block threshold is a judgment call,
// and the metadata length caps are guesses about data nobody has measured. Turning
// enforcement on before running this would mean discovering the false-positive rate by
// noticing memories had gone missing.
//
// The scan is read-only and never writes a decision back. Its output is a
// distribution, not a verdict on any one fact.
func runScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	namespace := fs.String("namespace", cliConfig.Namespace, "namespace")
	subject := fs.String("subject", "", "limit the scan to one subject")
	limit := fs.Int("limit", 0, "max facts to scan (0 = all)")
	withModel := fs.Bool("model", false, "also run the model screen (slow; needs a generator configured)")
	threat := fs.Int("threat", screening.DefaultPolicy().BlockThreat, "block threshold to report against")
	showTop := fs.Int("top", 15, "how many highest-scoring facts to list")
	format := fs.String("format", "text", "output format: text|json")
	timeout := fs.Duration("timeout", screening.DefaultTimeout, "per-fact model screen timeout")
	fs.Parse(args)

	store, closeStore, err := openStore(*dbPath, *namespace)
	if err != nil {
		log.Fatal(err)
	}
	if store == nil {
		return
	}
	defer closeStore()

	facts, err := store.List(context.Background(), memstore.QueryOpts{
		Subject:    *subject,
		OnlyActive: true,
		Limit:      *limit,
	})
	if err != nil {
		log.Fatalf("scan: %v", err)
	}
	if len(facts) == 0 {
		fmt.Fprintln(os.Stderr, "scan: no facts to scan")
		return
	}

	var gen screening.Generator
	if *withModel {
		g, err := buildScanGenerator()
		if err != nil {
			log.Fatalf("scan: --model requested but no generator available: %v", err)
		}
		gen = g
	}

	pol := screening.DefaultPolicy()
	pol.BlockThreat = *threat
	// Shadow mode: the scan reports what enforcement would do, and must never be the
	// thing that decides a fact's fate.
	pol.Enforce = false
	sc := screening.NewScreener(pol, gen, nil)
	sc.SetTimeout(*timeout)

	rep := scanCorpus(context.Background(), sc, facts, *threat)

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			log.Fatalf("scan: %v", err)
		}
		return
	}
	rep.writeText(os.Stdout, *showTop, *withModel)
}

// scanRow is one fact's result, carrying no content -- only the identifiers needed to
// go look at it and the scores needed to rank it.
type scanRow struct {
	ID          int64    `json:"id"`
	Subject     string   `json:"subject"`
	Category    string   `json:"category"`
	DetectScore int      `json:"detect_score"`
	DetectRules []string `json:"detect_rules,omitempty"`
	Obfuscated  bool     `json:"obfuscated,omitempty"`
	Threat      int      `json:"threat,omitempty"`
	ThreatCat   string   `json:"threat_category,omitempty"`
	WouldBlock  bool     `json:"would_block"`
	Unscreened  bool     `json:"unscreened,omitempty"`
}

type scanReport struct {
	Facts         int         `json:"facts"`
	WouldBlock    int         `json:"would_block"`
	Unscreened    int         `json:"unscreened"`
	DetectBuckets []int       `json:"detect_buckets"` // counts for 0, 1-19, 20-49, 50-79, 80+
	ThreatCounts  map[int]int `json:"threat_counts,omitempty"`
	MetaOverCap   int         `json:"metadata_values_over_inline_cap"`
	MetaMaxRunes  int         `json:"metadata_longest_value_runes"`
	MetaNonScalar int         `json:"metadata_non_scalar_values"`
	Rows          []scanRow   `json:"rows"`
}

func scanCorpus(ctx context.Context, sc *screening.Screener, facts []memstore.Fact, threat int) scanReport {
	rep := scanReport{
		Facts:         len(facts),
		DetectBuckets: make([]int, 5),
		ThreatCounts:  map[int]int{},
	}

	for _, f := range facts {
		d := sc.Screen(ctx, f.Content)

		row := scanRow{
			ID:          f.ID,
			Subject:     f.Subject,
			Category:    f.Category,
			DetectScore: d.DetectScore,
			DetectRules: d.DetectRules,
			Obfuscated:  d.Obfuscated,
			Threat:      d.Threat,
			ThreatCat:   d.Category,
		}
		if d.ModelScreened {
			rep.ThreatCounts[d.Threat]++
			if d.Verified && d.Threat >= threat {
				row.WouldBlock = true
				rep.WouldBlock++
			}
		} else {
			row.Unscreened = true
			rep.Unscreened++
		}
		rep.DetectBuckets[detectBucket(d.DetectScore)]++

		measureMetadata(f.Metadata, &rep)
		rep.Rows = append(rep.Rows, row)
	}

	// Rank by what an operator needs to look at first: would-be blocks, then the
	// strongest evidence.
	sort.SliceStable(rep.Rows, func(i, j int) bool {
		a, b := rep.Rows[i], rep.Rows[j]
		if a.WouldBlock != b.WouldBlock {
			return a.WouldBlock
		}
		if a.Threat != b.Threat {
			return a.Threat > b.Threat
		}
		return a.DetectScore > b.DetectScore
	})
	return rep
}

func detectBucket(score int) int {
	switch {
	case score == 0:
		return 0
	case score < 20:
		return 1
	case score < 50:
		return 2
	case score < 80:
		return 3
	default:
		return 4
	}
}

// measureMetadata records the shape statistics that the length caps need. It walks
// only the top level, matching how the renderer decides inline-vs-fenced.
func measureMetadata(raw json.RawMessage, rep *scanReport) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		rep.MetaNonScalar++
		return
	}
	for _, v := range m {
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			rep.MetaNonScalar++
			continue
		}
		s, ok := val.(string)
		if !ok {
			if _, isMap := val.(map[string]any); isMap {
				rep.MetaNonScalar++
			} else if _, isArr := val.([]any); isArr {
				rep.MetaNonScalar++
			}
			continue
		}
		n := len([]rune(s))
		if n > rep.MetaMaxRunes {
			rep.MetaMaxRunes = n
		}
		if n > fence.InlineValueMaxRunes {
			rep.MetaOverCap++
		}
	}
}

func (r scanReport) writeText(w *os.File, top int, withModel bool) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	p("scanned %d active facts\n\n", r.Facts)

	p("detect score distribution (regex; corroboration only, never blocks)\n")
	labels := []string{"       0", "    1-19", "   20-49", "   50-79", "     80+"}
	for i, n := range r.DetectBuckets {
		p("  %s  %5d  %s\n", labels[i], n, bar(n, r.Facts))
	}
	p("\n")

	if withModel {
		p("model threat distribution (the only blocking signal)\n")
		keys := make([]int, 0, len(r.ThreatCounts))
		for k := range r.ThreatCounts {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			p("  threat %2d  %5d  %s\n", k, r.ThreatCounts[k], bar(r.ThreatCounts[k], r.Facts))
		}
		p("\n  would block: %d of %d (%.2f%%)\n", r.WouldBlock, r.Facts, pct(r.WouldBlock, r.Facts))
		if r.Unscreened > 0 {
			p("  unscreened (model failed): %d\n", r.Unscreened)
		}
	} else {
		p("model screen not run; pass --model to get the blocking signal.\n")
		p("detect alone decides nothing, so this run cannot tell you the block rate.\n")
	}
	p("\n")

	p("metadata shape (informs the write-side length cap)\n")
	p("  longest string value:        %d runes\n", r.MetaMaxRunes)
	p("  values over the %d-rune cap: %d\n", fence.InlineValueMaxRunes, r.MetaOverCap)
	p("  non-scalar values:           %d\n", r.MetaNonScalar)
	p("\n")

	if top > 0 && len(r.Rows) > 0 {
		p("highest-scoring facts (review these for false positives)\n")
		for i, row := range r.Rows {
			if i >= top {
				break
			}
			if row.DetectScore == 0 && row.Threat == 0 {
				break
			}
			flag := " "
			if row.WouldBlock {
				flag = "!"
			}
			p("  %s id=%-6d detect=%-3d threat=%-2d %-18s %s\n",
				flag, row.ID, row.DetectScore, row.Threat,
				truncate(row.Subject, 18), strings.Join(row.DetectRules, ","))
		}
		p("\n  '!' marks facts enforcement would have rejected.\n")
		p("  Inspect one with: memstore list --subject <subject>\n")
	}
}

func bar(n, total int) string {
	if total == 0 {
		return ""
	}
	const width = 40
	w := n * width / total
	return strings.Repeat("#", w)
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

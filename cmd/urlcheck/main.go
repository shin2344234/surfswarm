// urlcheck fetches every URL in a list once, the same way the agent does, and
// reports which ones fail and why. Run it before a release to prune the
// bundled lists, or after a test to see which sites produced the errors. The
// same check is available from the server's URL lists page.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"surfswarm/internal/engine"
)

func main() {
	file := flag.String("file", "internal/server/data/browse_urls.txt", "URL list, one per line, # comments allowed")
	conc := flag.Int("concurrency", 8, "parallel fetches")
	timeout := flag.Duration("timeout", 15*time.Second, "per-request timeout")
	maxBytes := flag.Int64("max-bytes", 0, "stop reading a body after this many bytes (0 = read it all); useful for download lists")
	all := flag.Bool("all", false, "print every URL, not only the failures")
	emitOK := flag.String("emit-ok", "", "write the URLs that passed to this file, one per line, in input order")
	flag.Parse()

	urls, err := readList(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	client := &http.Client{Transport: engine.NewTransport(*conc)}

	results := make([]engine.CheckResult, len(urls))
	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, u string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = engine.CheckURL(client, u, *timeout, *maxBytes)
		}(i, u)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, r := range results {
		if r.Class == "" {
			counts["ok"]++
		} else {
			counts[r.Class]++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return counts[keys[i]] > counts[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	fmt.Printf("%d URLs: %s\n\n", len(urls), strings.Join(parts, ", "))

	var passed []string
	for _, r := range results {
		if r.Class == "" {
			passed = append(passed, r.URL)
		}
		if r.Class == "" && !*all {
			continue
		}
		class := r.Class
		if class == "" {
			class = "ok"
		}
		line := fmt.Sprintf("%-9s %3d %6.0fms %10dB %-24s %s", class, r.Status, r.Ms, r.Length, shortType(r.Type), r.URL)
		if r.FinalURL != "" && r.FinalURL != r.URL {
			line += "  -> " + r.FinalURL
		}
		if r.Message != "" {
			line += "\n          " + r.Message
		}
		fmt.Println(line)
	}
	if *emitOK != "" {
		if err := os.WriteFile(*emitOK, []byte(strings.Join(passed, "\n")+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("\nwrote %d passing URLs to %s\n", len(passed), *emitOK)
	}
}

func shortType(ct string) string {
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	if len(ct) > 24 {
		ct = ct[:24]
	}
	return ct
}

func readList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var urls []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, sc.Err()
}

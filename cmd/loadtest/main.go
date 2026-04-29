// Package main hammers the banking HTTP API with concurrent bidirectional transfers
// and asserts the balance-conservation invariant at the end.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type result struct{ errCode string } // empty errCode means success; see worker()

func getBalance(client *http.Client, baseURL, id string) (decimal.Decimal, error) {
	resp, err := client.Get(baseURL + "/api/accounts/" + id)
	if err != nil {
		return decimal.Zero, fmt.Errorf("getBalance: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, fmt.Errorf("getBalance: status %d", resp.StatusCode)
	}
	var body struct {
		Balance string `json:"Balance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return decimal.Zero, fmt.Errorf("getBalance decode: %w", err)
	}
	d, err := decimal.NewFromString(body.Balance)
	if err != nil {
		return decimal.Zero, fmt.Errorf("getBalance parse %q: %w", body.Balance, err)
	}
	return d, nil
}

func postTransfer(client *http.Client, baseURL, src, dst, amount, idemKey string) (int, string, error) {
	body := fmt.Sprintf(`{"src_account_id":%q,"dst_account_id":%q,"amount":%q}`, src, dst, amount)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/transfers", strings.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("postTransfer build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idemKey)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("postTransfer do: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusCreated {
		return resp.StatusCode, "", nil
	}
	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(bytes.NewReader(b)).Decode(&errBody)
	return resp.StatusCode, errBody.Error, nil
}

func worker(ctx context.Context, client *http.Client, baseURL, src, dst, amount string, iters int, out chan<- result) { // alternates direction to keep balances bounded
	for i := 0; i < iters; i++ {
		if ctx.Err() != nil {
			return
		}
		from, to := src, dst
		if i%2 == 1 {
			from, to = dst, src
		}
		_, errCode, err := postTransfer(client, baseURL, from, to, amount, uuid.New().String())
		if err != nil {
			out <- result{errCode: "http_error"}
			continue
		}
		out <- result{errCode: errCode}
	}
}

func main() {
	baseURL := flag.String("base-url", "http://localhost:8080", "service base URL")
	workers := flag.Int("workers", 8, "concurrent workers")
	iters := flag.Int("iters", 50, "transfers per worker")
	amount := flag.String("amount", "1.00", "transfer amount")
	src := flag.String("src", "00000000-0000-0000-0000-000000000002", "source account UUID")
	dst := flag.String("dst", "00000000-0000-0000-0000-000000000003", "destination account UUID")
	timeout := flag.Duration("timeout", 60*time.Second, "total test timeout")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	startSrc, err := getBalance(client, *baseURL, *src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	startDst, err := getBalance(client, *baseURL, *dst)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	startTotal := startSrc.Add(startDst)

	totalOps := *workers * *iters
	ch := make(chan result, totalOps)
	start := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(ctx, client, *baseURL, *src, *dst, *amount, *iters, ch)
		}()
	}
	wg.Wait()
	close(ch)
	elapsed := time.Since(start)

	// Collect results (single goroutine after close — no mutex needed).
	var successes int
	errCounts := make(map[string]int)
	for r := range ch {
		if r.errCode == "" {
			successes++
		} else {
			errCounts[r.errCode]++
		}
	}

	endSrc, err := getBalance(client, *baseURL, *src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	endDst, err := getBalance(client, *baseURL, *dst)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	endTotal := endSrc.Add(endDst)
	invariantOK := startTotal.Equal(endTotal)

	fmt.Println("load test summary")
	fmt.Printf("  target:                 %s\n", *baseURL)
	fmt.Printf("  src:                    %s\n", *src)
	fmt.Printf("  dst:                    %s\n", *dst)
	fmt.Printf("  workers x iters:        %d x %d = %d ops\n", *workers, *iters, totalOps)
	fmt.Printf("  elapsed:                %.2fs\n", elapsed.Seconds())
	fmt.Printf("  throughput:             %.1f ops/s\n", float64(totalOps)/elapsed.Seconds())
	fmt.Printf("  successes:              %d\n", successes)
	if len(errCounts) == 0 {
		fmt.Println("  failures by error_code: (none)")
	} else {
		for code, n := range errCounts {
			fmt.Printf("  failures [%s]: %d\n", code, n)
		}
	}
	fmt.Printf("  start balance src+dst:  %s\n", startTotal.StringFixed(4))
	fmt.Printf("  end balance src+dst:    %s\n", endTotal.StringFixed(4))
	if invariantOK {
		fmt.Println("  invariant:              PASS")
	} else {
		fmt.Println("  invariant:              FAIL")
	}

	if !invariantOK || len(errCounts) > 0 {
		os.Exit(1)
	}
}

// This is meant to be a software that can crawl websites for in-
// and outbound links, verifying that every link gives a 2xx-response
// back. If a link returns responses in the 3xx, 4xx or 5xx ranges,
// the software shall print that to console and/or log file for
// the user to handle later.

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type RequestError struct {
	err        error
	originURL  string
	originText string
}

type Link struct {
	originURL  string
	originText string
	url        string
}

type CrawlResponse struct {
	originURL  string
	originText string
	url        string
	statusCode int
	isOk       bool
}

const maxConcurrentURLChecks = 10
const crawlerUserAgent = "Golang Link Crawler/1.0"
const httpRequestMethod = "HEAD"
const httpRequestTimeout = 60 * time.Second

func main() {
	cliEntrypoint := flag.String("url", "", "Entrypoint URL")
	cliConcurrentLimit := flag.Int("limit", maxConcurrentURLChecks, "Limit amount of concurrent scrapes")
	cliRequestMethod := flag.String("method", httpRequestMethod, "Initial method, HEAD or GET")
	cliTimeout := flag.Duration("timeout", httpRequestTimeout, "Timeout limit for each request")
	cliVerify := flag.Bool("verify", true, "Ask user to verify crawl before continuing.")
	flag.Parse()

	var entrypoint string
	if *cliEntrypoint != "" {
		entrypoint = *cliEntrypoint
	} else {
		fmt.Print("Enter sitemap URL: ")
		fmt.Scanln(&entrypoint)
	}

	concurrentLimit := *cliConcurrentLimit
	requestMethod := *cliRequestMethod
	timeout := *cliTimeout
	verifyTest := *cliVerify

	start := time.Now()
	timestamp := start.Unix()

	parsedEntrypoint, err := url.ParseRequestURI(entrypoint)
	if err != nil {
		log.Fatal(err)
	}

	crawlURLs, err := getSitemap(entrypoint, concurrentLimit, timeout)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	httpClient := &http.Client{
		Timeout:       timeout,
		CheckRedirect: redirectTrim,
	}
	defer httpClient.CloseIdleConnections()

	var (
		allLinks []Link
		linksMu  sync.Mutex
		seenURLs = make(map[string]bool)
	)

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrentLimit)

	for _, crawlURL := range crawlURLs {
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			pageLinks := getPageLinks(u, httpClient)
			linksMu.Lock()
			for _, link := range pageLinks {
				if !seenURLs[link.url] {
					seenURLs[link.url] = true
					allLinks = append(allLinks, link)
				}
			}
			linksMu.Unlock()
		}(crawlURL)
	}
	wg.Wait()

	fmt.Println("A total of", len(allLinks), "links were found in", len(crawlURLs), "pages")

	if verifyTest {
		var userContinue string
		fmt.Print("Continue verifying URLs? (y/n) ")
		fmt.Scan(&userContinue)
		if strings.ToLower(userContinue) != "y" {
			os.Exit(1)
		}
	}
	fmt.Println()

	crawledURLs, urlErrors, requestErrors := checkURLStatus(allLinks, concurrentLimit, requestMethod, timeout)
	numErrors := len(urlErrors)

	var logFileName string
	if numErrors > 0 || len(requestErrors) > 0 {
		if _, err := os.Stat("./logs"); os.IsNotExist(err) {
			if err := os.Mkdir("./logs", 0755); err != nil {
				log.Fatal(err)
			}
		}
		logFileName = "logs/result_" + parsedEntrypoint.Host + "_" + strconv.FormatInt(timestamp, 10) + ".log"
		file, err := os.OpenFile(logFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("Error opening file: %v\n", err)
		}
		defer file.Close()
		log.SetOutput(file)
	}

	fmt.Println()
	if len(requestErrors) > 0 {
		fmt.Println("Errors raised while checking URLs")
		for _, e := range requestErrors {
			log.Printf("%v (linked from %v with text %v)\n", e.err, e.originURL, e.originText)
		}
	}

	if numErrors > 0 {
		for _, item := range urlErrors {
			log.Printf("HTTP %d for %s (linked from %s with text %s)\n", item.statusCode, item.url, item.originURL, item.originText)
		}
	}

	fmt.Printf("\nA total of %d links on %d pages was checked and %d produced errors of some sort.\n", len(crawledURLs), len(crawlURLs), numErrors)
	fmt.Println("Total execution time:", time.Since(start))

	if logFileName != "" {
		fmt.Printf("\nHTTP errors found. Check logfile (%v) for results.\n", logFileName)
	}
}

func redirectTrim(req *http.Request, via []*http.Request) error {
	if len(via) >= 25 {
		return errors.New("stopped after 25 redirects")
	}
	return nil
}

func checkURLStatus(links []Link, concurrentLimit int, requestMethod string, timeout time.Duration) ([]CrawlResponse, []CrawlResponse, []RequestError) {
	client := &http.Client{
		CheckRedirect: redirectTrim,
		Timeout:       timeout,
	}
	defer client.CloseIdleConnections()

	var (
		crawledURLs   []CrawlResponse
		retryURLs     []Link
		requestErrors []RequestError
		mu            sync.Mutex
	)

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrentLimit)

	for _, link := range links {
		wg.Add(1)
		sem <- struct{}{}
		go func(input Link) {
			defer wg.Done()
			defer func() { <-sem }()

			method := http.MethodHead
			if requestMethod == "GET" {
				method = http.MethodGet
			}

			req, err := http.NewRequest(method, input.url, nil)
			if err != nil {
				fmt.Println(err)
				return
			}
			req.Header.Set("User-Agent", crawlerUserAgent)

			resp, err := client.Do(req)
			if err != nil {
				fmt.Println("Request error:", err)
				mu.Lock()
				retryURLs = append(retryURLs, input)
				mu.Unlock()
				return
			}
			defer resp.Body.Close()

			statusCode := resp.StatusCode
			// Treat LinkedIn's non-standard 999 as OK
			isOk := (statusCode >= 200 && statusCode <= 299) || statusCode == 999
			fmt.Printf("%s response %d for %s\n", requestMethod, statusCode, input.url)

			mu.Lock()
			crawledURLs = append(crawledURLs, CrawlResponse{
				originURL:  input.originURL,
				originText: input.originText,
				url:        input.url,
				statusCode: statusCode,
				isOk:       isOk,
			})
			mu.Unlock()
		}(link)
	}
	wg.Wait()

	// Retry with GET for any URLs that failed the initial request
	if len(retryURLs) > 0 {
		retryClient := &http.Client{Timeout: timeout}
		defer retryClient.CloseIdleConnections()

		var retryWg sync.WaitGroup
		retrySem := make(chan struct{}, concurrentLimit)

		for _, link := range retryURLs {
			retryWg.Add(1)
			retrySem <- struct{}{}
			go func(input Link) {
				defer retryWg.Done()
				defer func() { <-retrySem }()

				req, err := http.NewRequest(http.MethodGet, input.url, nil)
				if err != nil {
					fmt.Println("GET error:", err)
					return
				}
				req.Header.Set("User-Agent", crawlerUserAgent)

				resp, err := retryClient.Do(req)
				if err != nil {
					fmt.Println(err)
					mu.Lock()
					requestErrors = append(requestErrors, RequestError{
						err:        err,
						originURL:  input.originURL,
						originText: input.originText,
					})
					mu.Unlock()
					return
				}
				defer resp.Body.Close()

				statusCode := resp.StatusCode
				isOk := statusCode >= 200 && statusCode <= 299
				fmt.Printf("GET response %d for %s\n", statusCode, input.url)

				mu.Lock()
				crawledURLs = append(crawledURLs, CrawlResponse{
					originURL:  input.originURL,
					originText: input.originText,
					url:        input.url,
					statusCode: statusCode,
					isOk:       isOk,
				})
				mu.Unlock()
			}(link)
		}
		retryWg.Wait()
	}

	var urlErrors []CrawlResponse
	for _, item := range crawledURLs {
		if !item.isOk {
			urlErrors = append(urlErrors, item)
		}
	}

	return crawledURLs, urlErrors, requestErrors
}

// getPageLinks fetches a page and returns all unique HTTP(S) links found in it.
func getPageLinks(inputURL string, client *http.Client) []Link {
	parsedBase, err := url.Parse(inputURL)
	if err != nil {
		fmt.Printf("Failed to parse URL %s: %v\n", inputURL, err)
		return nil
	}

	fmt.Println("Link scraping:", inputURL)

	req, err := http.NewRequest(http.MethodGet, inputURL, nil)
	if err != nil {
		fmt.Printf("Failed to create request for %s: %v\n", inputURL, err)
		return nil
	}
	req.Header.Set("User-Agent", crawlerUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Failed to fetch %s: %v\n", inputURL, err)
		return nil
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		fmt.Printf("Failed to parse HTML from %s: %v\n", inputURL, err)
		return nil
	}

	var links []Link
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		linkURL, exists := s.Attr("href")
		if !exists {
			return
		}
		linkText := strings.TrimSpace(s.Text())

		parsedLink, err := url.Parse(linkURL)
		if err != nil {
			return
		}

		// Resolve relative URLs against the page base
		resolved := parsedBase.ResolveReference(parsedLink)

		// Skip non-HTTP schemes (mailto:, tel:, javascript:, etc.)
		if resolved.Scheme != "http" && resolved.Scheme != "https" {
			return
		}

		// Strip fragments — #section links point to the same resource
		resolved.Fragment = ""

		links = append(links, Link{
			originURL:  inputURL,
			originText: linkText,
			url:        resolved.String(),
		})
	})

	return links
}

func getSitemap(entrypoint string, concurrentLimit int, timeout time.Duration) ([]string, error) {
	res, err := getXML(entrypoint, timeout)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse sitemap XML: %w", err)
	}

	return parseSitemap(*doc, concurrentLimit, timeout), nil
}

func getXML(entrypoint string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, entrypoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", crawlerUserAgent)
	return client.Do(req)
}

// parseURLSet extracts all <loc> text values from a sitemap document.
func parseURLSet(doc goquery.Document) []string {
	var locations []string
	doc.Find("loc").Each(func(_ int, s *goquery.Selection) {
		if loc := strings.TrimSpace(s.Text()); loc != "" {
			locations = append(locations, loc)
		}
	})
	return locations
}

func parseSitemap(doc goquery.Document, concurrentLimit int, timeout time.Duration) []string {
	if len(doc.Find("sitemap").Nodes) > 0 {
		// Sitemap index: fetch each child sitemap concurrently
		sitemapURLs := parseURLSet(doc)
		var (
			pages []string
			mu    sync.Mutex
			wg    sync.WaitGroup
		)
		sem := make(chan struct{}, concurrentLimit)

		for _, ep := range sitemapURLs {
			wg.Add(1)
			sem <- struct{}{}
			go func(ep string) {
				defer wg.Done()
				defer func() { <-sem }()
				result, err := getSitemap(ep, concurrentLimit, timeout)
				if err != nil {
					fmt.Println(err)
					return
				}
				mu.Lock()
				pages = append(pages, result...)
				mu.Unlock()
			}(ep)
		}
		wg.Wait()

		// Deduplicate across child sitemaps
		seen := make(map[string]bool)
		deduped := make([]string, 0, len(pages))
		for _, p := range pages {
			if !seen[p] {
				seen[p] = true
				deduped = append(deduped, p)
			}
		}
		return deduped
	} else if len(doc.Find("url").Nodes) > 0 {
		return parseURLSet(doc)
	}

	fmt.Println("Empty result")
	return nil
}

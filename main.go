// This is meant to be a software that can crawl websites for in-
// and outbound links, verifying that every link gives a 2xx-response
// back. If a link returns responses in the 3xx, 4xx or 5xx ranges,
// the software shall print that to console and/or log file for
// the user to handle later.

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gocolly/colly"
)

// Struct for giving request errors more information
type RequestErrors struct {
	err         error
	origin_url  string
	origin_text string
}

type Link struct {
	origin_url  string
	origin_text string
	url         string
}

type CrawlResponse struct {
	origin_url  string
	origin_text string
	url         string
	status_code int
	is_ok       bool
}

// Set a maximum of concurrent jobs
const MAX_CONCURRENT_SCRAPES = 50
const MAX_CONCURRENT_URLCHECKS = 10

// Set constant for User Agent
const CRAWLER_USER_AGENT = "Golang Link Crawler/1.0"

// Set a constant for HTTP request timeout
const HTTP_REQUEST_TIMEOUT = time.Duration(time.Second * 30)

// Init empty slice of URLs to verify
var url_list []Link

// Init empty slice of pages to crawl
var page_list []string

// Init empty slice for crawled URLs
var crawled_urls []CrawlResponse

var num_errors int = 0
var url_errors []CrawlResponse
var request_errors []RequestErrors

// Global variables for flag
var entrypoint string
var concurrent_limit int
var timeout time.Duration
var verify_test bool

// Main function for executing the program
func main() {
	// Parse CLI flags
	cli_entrypoint := flag.String("url", "", "Entrypoint URL")
	cli_concurrent_limit := flag.Int("limit", MAX_CONCURRENT_URLCHECKS, "Limit amount of concurrent scrapes")
	cli_timeout := flag.Duration("timeout", HTTP_REQUEST_TIMEOUT, "Timeout limit for each request")
	cli_verify := flag.Bool("verify", true, "Ask user to verify crawl before continuing.")
	flag.Parse()
	if *cli_entrypoint != "" {
		entrypoint = *cli_entrypoint
	} else {
		fmt.Print("Enter sitemap URL: ")
		fmt.Scanln(&entrypoint)
	}
	concurrent_limit = *cli_concurrent_limit
	timeout = *cli_timeout
	verify_test = *cli_verify
	// Init start time for execution time calc
	start := time.Now()
	timestamp := start.Unix()

	// Parse entry point for base url
	parsed_entrypoint, err := url.ParseRequestURI(entrypoint)
	if err != nil {
		log.Fatal(err)
	}

	// Get sitemap content
	crawl_urls, err := getSitemap(entrypoint)
	if err != nil {
		fmt.Println(err)
		os.Exit(0)
	}

	var links []Link
	queue := make(chan bool, concurrent_limit)
	for _, crawl_url := range crawl_urls {
		queue <- true
		go func(crawl_url string) {
			defer func() { <-queue }()
			// Get all links in page
			links = getPageLinks(crawl_url)
		}(crawl_url)
	}
	for i := 0; i < concurrent_limit; i++ {
		queue <- true
	}

	fmt.Println("A total of", len(links), "links were found in", len(crawl_urls), "pages")
	// Ask user to continue before verifying URLs
	if verify_test {
		var user_continue string
		fmt.Print("Continue verifying URLs? (y/n) ")
		fmt.Scan(&user_continue)
		if strings.ToLower(user_continue) != "y" {
			os.Exit(1)
		}
	}
	fmt.Println()

	// Check all links from all pages
	checkUrlStatus(links)

	// Output request errors at end of script
	defer func() {
		if num_errors > 0 || len(request_errors) > 0 {
			// Set up file for log
			if _, err := os.Stat("./logs"); os.IsNotExist(err) {
				err := os.Mkdir("./logs", 0755)
				if err != nil {
					log.Fatal(err)
				}
			}
			file_name := "logs/result_" + parsed_entrypoint.Host + "_" + strconv.Itoa(int(timestamp)) + ".log"
			file, err := os.OpenFile(file_name, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
			if err != nil {
				log.Fatalf("Error opening file: %v\n", err)
			}
			defer file.Close()
			log.SetOutput(file)
			defer fmt.Printf("\nHTTP errors found. Check logfile (%v) for results.\n", file_name)
		}
		fmt.Println()
		if len(request_errors) > 0 {
			fmt.Println("\nErrors raised while checking URLs")
			for _, err := range request_errors {
				log.Printf("%v (linked from %v with text %v)\n", err.err, err.origin_url, err.origin_text)
			}
		}
		// Output HTTP errors
		fmt.Println()
		// Check if errors exists and output them to log file
		if num_errors > 0 {
			for _, item := range url_errors {
				log.Printf("HTTP %d for %s (linked from %s with text %s)\n", item.status_code, item.url, item.origin_url, item.origin_text)
			}
		}
		// End output
		fmt.Println("\nA total of", len(crawled_urls), "links on", len(crawl_urls), "pages was checked and", num_errors, "produced errors of some sort.")
		defer fmt.Println("\nTotal execution time:", time.Since(start))
	}()
}

// Function for making a HEAD call and return status code
func checkUrlStatus(links []Link) {
	// Init default return value
	status := 0
	client := &http.Client{Timeout: timeout}
	defer client.CloseIdleConnections()
	// Slice for links with errors
	var retry_urls []Link

	queue := make(chan bool, concurrent_limit)
	for _, link := range links {
		queue <- true
		go func(input Link) {
			defer func() { <-queue }()
			req, err := http.NewRequest(http.MethodHead, input.url, nil)
			if err != nil {
				fmt.Println(err)
			}

			req.Header.Set("User-Agent", CRAWLER_USER_AGENT)
			resp, err := client.Do(req)
			if err != nil {
				retry_urls = append(retry_urls, input)
				//request_errors = append(request_errors, RequestErrors{err: err, origin_url: input.origin_url, origin_text: input.origin_text})
				fmt.Println("HEAD error:", err)
			}
			if err == nil {
				defer resp.Body.Close()
				status = resp.StatusCode
			}
			is_ok := false
			if status >= 200 && status <= 299 {
				is_ok = true
			}
			fmt.Printf("HEAD response %d for %s\n", status, input.url)
			crawled_urls = append(crawled_urls, CrawlResponse{origin_url: input.origin_url, origin_text: input.origin_text, url: input.url, status_code: status, is_ok: is_ok})

		}(link)
	}
	for i := 0; i < concurrent_limit; i++ {
		queue <- true
	}

	// Retry with GET instead of head if retry_urls is populated
	if len(retry_urls) > 0 {
		retry_client := &http.Client{Timeout: timeout}
		defer retry_client.CloseIdleConnections()
		retry_queue := make(chan bool, concurrent_limit)
		for _, link := range retry_urls {
			go func(input Link) {
				defer func() { <-retry_queue }()
				req, err := http.NewRequest(http.MethodGet, input.url, nil)
				if err != nil {
					fmt.Println("GET error:", err)
				}

				req.Header.Set("User-Agent", CRAWLER_USER_AGENT)
				resp, err := retry_client.Do(req)
				if err != nil {
					fmt.Println(err)
					request_errors = append(request_errors, RequestErrors{err: err, origin_url: input.origin_url, origin_text: input.origin_text})
				}
				if err == nil {
					defer resp.Body.Close()
					status = resp.StatusCode
				}
				is_ok := false
				if status >= 200 && status <= 299 {
					is_ok = true
				}
				fmt.Printf("GET response %d for %s\n", status, input.url)
				crawled_urls = append(crawled_urls, CrawlResponse{origin_url: input.origin_url, origin_text: input.origin_text, url: input.url, status_code: status, is_ok: is_ok})
			}(link)
		}
		for i := 0; i < concurrent_limit; i++ {
			retry_queue <- true
		}
	}
	// Check number of errors
	for _, item := range crawled_urls {
		if !item.is_ok {
			num_errors++
			url_errors = append(url_errors, item)
		}
	}
}

// Function for checking unique URLs
func isUniqueUrl(link_url string) bool {
	for _, exists := range url_list {
		// Check if url_list contains link_url and if so, return false for is_unique
		if exists.url == link_url {
			return false
		}
	}
	return true
}

func isUniquePage(page_url string) bool {
	for _, exists := range page_list {
		if exists == page_url {
			return false
		}
	}
	return true
}

// Function for fetching URLs in a HTML page, returning a list
// that can be crawled for status later
func getPageLinks(input_url string) []Link {
	// Parse input URL to URL object
	parsed_entrypoint, _ := url.ParseRequestURI(input_url)
	// Start up Colly
	c := colly.NewCollector()
	c.Limit(&colly.LimitRule{RandomDelay: 3 * time.Second})

	c.OnHTML("a[href]", func(h *colly.HTMLElement) {
		link_url := h.Attr("href")
		link_text := strings.TrimSpace(h.Text)
		parsed_url, _ := url.ParseRequestURI(link_url)
		if parsed_url != nil {
			// Check if URL has empty Scheme and if so, add base url from input
			if parsed_url.Scheme == "" {
				base_url := parsed_entrypoint.Scheme + "://" + parsed_entrypoint.Hostname()
				link_url = base_url + link_url
			}
			// Remove all # content
			trimmed_url1 := strings.Split(link_url, "#")
			trimmed_url2 := strings.Split(trimmed_url1[0], "&")
			// Check if URL is in list already
			is_unique := isUniqueUrl(trimmed_url2[0])
			if is_unique && parsed_url.Scheme != "mailto" && parsed_url.Scheme != "tel" && parsed_url.Scheme != "irc" && parsed_url.Scheme != "javascript" && parsed_url.Scheme != "skype" {
				// Append link to slice that will be returned from function
				url_list = append(url_list, Link{origin_url: input_url, origin_text: link_text, url: trimmed_url2[0]})
			}
		}
	})

	c.OnRequest(func(r *colly.Request) {
		fmt.Println("Link scraping: ", r.URL.String())
	})

	c.Visit(input_url)

	return url_list
}

// Function for listing URLs in sitemap, returning a list that
// can be crawled for status later
func getSitemap(entrypoint string) ([]string, error) {
	res, err := getXML(entrypoint)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		panic(err)
	}
	return parseSitemap(*doc), nil
}

func getXML(entrypoint string) (*http.Response, error) {
	// Go fetch!
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, entrypoint, nil)
	if err != nil {
		fmt.Println(err)
	}
	req.Header.Set("User-Agent", CRAWLER_USER_AGENT)
	res, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	return res, nil
}

func parseUrlset(doc goquery.Document) []string {
	locations := []string{}
	sel := doc.Find("loc")
	for i := range sel.Nodes {
		loc := sel.Eq(i)
		result := loc.Text()
		if isUniquePage(result) {
			page_list = append(page_list, result)
			locations = append(locations, result)
		}
	}

	return locations
}

func parseSitemap(doc goquery.Document) []string {
	// Check if sitemap file contains sitemap or url tags
	if len(doc.Find("sitemap").Nodes) > 0 {
		queue := make(chan bool, concurrent_limit)
		sitemaps := parseUrlset(doc)
		var pages []string
		for _, entrypoint := range sitemaps {
			queue <- true
			go func(entrypoint string) {
				defer func() { <-queue }()
				result, err := getSitemap(entrypoint)
				if err != nil {
					fmt.Println(err)
				}
				pages = append(pages, result...)
			}(entrypoint)
		}
		for i := 0; i < concurrent_limit; i++ {
			queue <- true
		}
		return pages
	} else if len(doc.Find("url").Nodes) > 0 {
		pages := parseUrlset(doc)
		return pages
	} else {
		fmt.Println("Empty result")
		return nil
	}
}

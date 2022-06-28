// This is meant to be a software that can crawl websites for in-
// and outbound links, verifying that every link gives a 2xx-response
// back. If a link returns responses in the 3xx, 4xx or 5xx ranges,
// the software shall print that to console and/or log file for
// the user to handle later.

package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gocolly/colly"
)

// type Sitemap struct {
// 	origin string
// 	urls   []string
// }

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

// Temporary entry point if no arguments given
var entrypoint string = "http://127.0.0.1/sitemap.xml"

// Set a maximum of concurrent jobs
const MAX_CONCURRENT_JOBS = 100

// Init empty slice of URLs to verify
var url_list []Link

// Init empty slice for crawled URLs
var crawled_urls []CrawlResponse

var num_errors int = 0
var url_errors []CrawlResponse

// Main function for executing the program
func main() {
	if len(os.Args) == 2 {
		_, input_err := url.ParseRequestURI(os.Args[1])
		if input_err != nil {
			panic("Error in input URL!")
		}
		entrypoint = os.Args[1]
	}
	// Init start time for execution time calc
	start := time.Now()

	// Get sitemap content
	crawl_urls, err := getSitemap(entrypoint)
	if err != nil {
		log.Println(err)
	}

	// Wait group init
	var wg sync.WaitGroup
	var links []Link
	for _, crawl_url := range crawl_urls {
		wg.Add(1)
		go func(wg *sync.WaitGroup, crawl_url string) {
			defer wg.Done()
			// Get all links in page
			links = getPageLinks(crawl_url)
		}(&wg, crawl_url)
	}
	wg.Wait()

	fmt.Println("A total of", len(links), "links were found in", len(crawl_urls), "pages")
	fmt.Println()

	// Check all links from all pages
	checkUrlStatus(links)

	// Output at end of script
	fmt.Println()
	if num_errors > 0 {
		fmt.Println("\nErrors found:")
		for _, item := range url_errors {
			fmt.Printf("HTTP %d for %s (linked from %s with text %s)\n", item.status_code, item.url, item.origin_url, item.origin_text)
		}
	}
	fmt.Println("\nA total of", len(crawled_urls), "links was checked and", num_errors, "produced errors of some sort.")
	fmt.Println("\nTotal execution time:", time.Since(start))
}

// Function for making a HEAD call and return status code
func checkUrlStatus(links []Link) {
	// Init default return value
	status := 999
	client := &http.Client{Timeout: 60 * time.Second}

	// Wait group init
	var wg sync.WaitGroup
	for _, link := range links {
		req, err := http.NewRequest("HEAD", link.url, nil)
		if err != nil {
			log.Println(err)
		}

		req.Header.Set("User-Agent", "Golang Link Crawler/1.0")
		wg.Add(1)
		go func(wg *sync.WaitGroup, input Link) {
			defer wg.Done()
			resp, err := client.Do(req)
			if err != nil {
				if err, ok := err.(net.Error); ok && err.Timeout() {
					status = 408
				}
				if os.IsTimeout(err) {
					status = 408
				}
				log.Println(err)
			}
			if err == nil {
				status = resp.StatusCode
				defer resp.Body.Close()
			}
			is_ok := false
			if status >= 200 && status <= 299 {
				is_ok = true
			}
			fmt.Printf("HTTP %d for %s\n", status, input.url)
			crawled_urls = append(crawled_urls, CrawlResponse{origin_url: input.origin_url, origin_text: input.origin_text, url: input.url, status_code: status, is_ok: is_ok})
		}(&wg, link)
	}
	wg.Wait()
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

// Function for fetching URLs in a HTML page, returning a list
// that can be crawled for status later
func getPageLinks(input_url string) []Link {
	// Parse input URL to URL object
	parsed_entrypoint, _ := url.ParseRequestURI(input_url)
	// Start up Colly
	c := colly.NewCollector()
	c.Limit(&colly.LimitRule{RandomDelay: 1 * time.Second})

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
			if is_unique && parsed_url.Scheme != "mailto" && parsed_url.Scheme != "tel" && parsed_url.Scheme != "irc" {
				// Append link to slice that will be returned from function
				url_list = append(url_list, Link{origin_url: input_url, origin_text: link_text, url: trimmed_url2[0]})
			}
		}
	})

	c.OnRequest(func(r *colly.Request) {
		fmt.Println("Scraping", r.URL.String(), "for links.")
	})

	c.Visit(input_url)

	return url_list
}

// Function for listing URLs in sitemap, returning a list that
// can be crawled for status later
func getSitemap(entrypoint string) ([]string, error) {
	res, err := getXML(entrypoint)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	return parseSitemap(*doc), nil
}

func getXML(entrypoint string) (*http.Response, error) {
	// Go fetch!
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", entrypoint, nil)
	if err != nil {
		log.Println(err)
	}
	req.Header.Set("User-Agent", "Ewenson Link Crawler/1.0")
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
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
		locations = append(locations, result)
	}

	return locations
}

func parseSitemap(doc goquery.Document) []string {
	// Check if sitemap file contains sitemap or url tags
	if len(doc.Find("sitemap").Nodes) > 0 {
		var wg sync.WaitGroup
		sitemaps := parseUrlset(doc)
		var pages []string
		for _, entrypoint := range sitemaps {
			wg.Add(1)
			go func(wg *sync.WaitGroup, entrypoint string) {
				defer wg.Done()
				result, err := getSitemap(entrypoint)
				if err != nil {
					log.Println(err)
				}
				pages = append(pages, result...)
			}(&wg, entrypoint)
		}
		wg.Wait()
		return pages
	} else if len(doc.Find("url").Nodes) > 0 {
		pages := parseUrlset(doc)
		return pages
	} else {
		log.Fatal("Nothing found")
		return nil
	}
}

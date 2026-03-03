package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// makeDoc creates a goquery.Document from an XML/HTML string for use in tests.
func makeDoc(t *testing.T, xml string) goquery.Document {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("failed to create goquery document: %v", err)
	}
	return *doc
}

// ---- redirectTrim -------------------------------------------------------

func TestRedirectTrim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		numVia    int
		wantError bool
	}{
		{"no redirects", 0, false},
		{"one redirect", 1, false},
		{"24 redirects", 24, false},
		{"25 redirects — limit hit", 25, true},
		{"30 redirects", 30, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			via := make([]*http.Request, tt.numVia)
			err := redirectTrim(nil, via)
			if (err != nil) != tt.wantError {
				t.Errorf("redirectTrim() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

// ---- parseURLSet --------------------------------------------------------

func TestParseURLSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		xml      string
		wantLocs []string
	}{
		{
			name: "standard urlset with two entries",
			xml: `<?xml version="1.0" encoding="UTF-8"?>
<urlset>
  <url><loc>https://example.com/</loc></url>
  <url><loc>https://example.com/about</loc></url>
</urlset>`,
			wantLocs: []string{"https://example.com/", "https://example.com/about"},
		},
		{
			name:     "empty urlset",
			xml:      `<urlset></urlset>`,
			wantLocs: nil,
		},
		{
			name:     "loc with surrounding whitespace is trimmed",
			xml:      `<urlset><url><loc>  https://example.com/page  </loc></url></urlset>`,
			wantLocs: []string{"https://example.com/page"},
		},
		{
			name:     "empty loc element is skipped",
			xml:      `<urlset><url><loc></loc></url><url><loc>https://example.com/</loc></url></urlset>`,
			wantLocs: []string{"https://example.com/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := makeDoc(t, tt.xml)
			got := parseURLSet(doc)
			if len(got) != len(tt.wantLocs) {
				t.Fatalf("got %d locs, want %d\n  got:  %v\n  want: %v", len(got), len(tt.wantLocs), got, tt.wantLocs)
			}
			for i, loc := range got {
				if loc != tt.wantLocs[i] {
					t.Errorf("loc[%d] = %q, want %q", i, loc, tt.wantLocs[i])
				}
			}
		})
	}
}

// ---- parseSitemap -------------------------------------------------------

func TestParseSitemap_URLSet(t *testing.T) {
	t.Parallel()
	doc := makeDoc(t, `<?xml version="1.0" encoding="UTF-8"?>
<urlset>
  <url><loc>https://example.com/</loc></url>
  <url><loc>https://example.com/contact</loc></url>
</urlset>`)
	pages := parseSitemap(doc, 5, 5*time.Second)
	if len(pages) != 2 {
		t.Fatalf("expected 2 pages, got %d: %v", len(pages), pages)
	}
}

func TestParseSitemap_Empty(t *testing.T) {
	t.Parallel()
	doc := makeDoc(t, `<root></root>`)
	pages := parseSitemap(doc, 5, 5*time.Second)
	if len(pages) != 0 {
		t.Errorf("expected empty result, got %v", pages)
	}
}

func TestParseSitemap_SitemapIndex_FetchesChildSitemaps(t *testing.T) {
	t.Parallel()
	child1 := `<?xml version="1.0" encoding="UTF-8"?><urlset><url><loc>https://example.com/page1</loc></url></urlset>`
	child2 := `<?xml version="1.0" encoding="UTF-8"?><urlset><url><loc>https://example.com/page2</loc></url></urlset>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap1.xml":
			fmt.Fprint(w, child1)
		case "/sitemap2.xml":
			fmt.Fprint(w, child2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	indexXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex>
  <sitemap><loc>%s/sitemap1.xml</loc></sitemap>
  <sitemap><loc>%s/sitemap2.xml</loc></sitemap>
</sitemapindex>`, srv.URL, srv.URL)

	doc := makeDoc(t, indexXML)
	pages := parseSitemap(doc, 5, 5*time.Second)

	if len(pages) != 2 {
		t.Fatalf("expected 2 pages from index, got %d: %v", len(pages), pages)
	}
}

func TestParseSitemap_SitemapIndex_DeduplicatesPages(t *testing.T) {
	t.Parallel()
	// Both child sitemaps return the same URL — only one should survive.
	child := `<?xml version="1.0" encoding="UTF-8"?><urlset><url><loc>https://example.com/same</loc></url></urlset>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, child)
	}))
	defer srv.Close()

	indexXML := fmt.Sprintf(`<sitemapindex>
  <sitemap><loc>%s/s1.xml</loc></sitemap>
  <sitemap><loc>%s/s2.xml</loc></sitemap>
</sitemapindex>`, srv.URL, srv.URL)

	doc := makeDoc(t, indexXML)
	pages := parseSitemap(doc, 5, 5*time.Second)

	if len(pages) != 1 {
		t.Errorf("expected 1 deduplicated page, got %d: %v", len(pages), pages)
	}
}

// ---- getPageLinks -------------------------------------------------------

func TestGetPageLinks_ReturnsAbsoluteAndRelativeLinks(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body>
			<a href="/about">About</a>
			<a href="https://external.com/page">External</a>
		</body></html>`)
	}))
	defer srv.Close()

	links := getPageLinks(srv.URL+"/", srv.Client())
	if len(links) != 2 {
		t.Fatalf("expected 2 links, got %d: %v", len(links), links)
	}
}

func TestGetPageLinks_ResolvesRelativeURLs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body>
			<a href="/contact">Absolute path</a>
			<a href="sub/page">Relative path</a>
		</body></html>`)
	}))
	defer srv.Close()

	links := getPageLinks(srv.URL+"/base/", srv.Client())
	for _, link := range links {
		if !strings.HasPrefix(link.url, "http") {
			t.Errorf("expected fully resolved URL, got %q", link.url)
		}
	}
}

func TestGetPageLinks_FiltersNonHTTPSchemes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body>
			<a href="mailto:user@example.com">Email</a>
			<a href="tel:+1234567890">Phone</a>
			<a href="javascript:void(0)">JS</a>
			<a href="irc://irc.example.com/channel">IRC</a>
			<a href="https://example.com/valid">Valid</a>
		</body></html>`)
	}))
	defer srv.Close()

	links := getPageLinks(srv.URL+"/", srv.Client())
	if len(links) != 1 {
		t.Errorf("expected 1 link (only https), got %d: %v", len(links), links)
	}
	if len(links) > 0 && links[0].url != "https://example.com/valid" {
		t.Errorf("unexpected link URL: %q", links[0].url)
	}
}

func TestGetPageLinks_StripsFragments(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body>
			<a href="/page#section">Section link</a>
		</body></html>`)
	}))
	defer srv.Close()

	links := getPageLinks(srv.URL+"/", srv.Client())
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
	if strings.Contains(links[0].url, "#") {
		t.Errorf("expected fragment to be stripped, got %q", links[0].url)
	}
}

func TestGetPageLinks_SetsOriginFields(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body><a href="/page">Click here</a></body></html>`)
	}))
	defer srv.Close()

	pageURL := srv.URL + "/"
	links := getPageLinks(pageURL, srv.Client())
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
	if links[0].originURL != pageURL {
		t.Errorf("originURL = %q, want %q", links[0].originURL, pageURL)
	}
	if links[0].originText != "Click here" {
		t.Errorf("originText = %q, want %q", links[0].originText, "Click here")
	}
}

func TestGetPageLinks_NetworkError_ReturnsNil(t *testing.T) {
	t.Parallel()
	// Point at a port that refuses connections.
	links := getPageLinks("http://127.0.0.1:1", http.DefaultClient)
	if links != nil {
		t.Errorf("expected nil on network error, got %v", links)
	}
}

func TestGetPageLinks_EmptyPage_ReturnsNil(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body></body></html>`)
	}))
	defer srv.Close()

	links := getPageLinks(srv.URL+"/", srv.Client())
	if len(links) != 0 {
		t.Errorf("expected 0 links, got %d", len(links))
	}
}

// ---- checkURLStatus -----------------------------------------------------

func TestCheckURLStatus_AllOK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	links := []Link{
		{originURL: "https://example.com/", originText: "Home", url: srv.URL + "/"},
		{originURL: "https://example.com/", originText: "About", url: srv.URL + "/about"},
	}

	crawled, urlErrors, requestErrors := checkURLStatus(links, 5, "HEAD", 5*time.Second)

	if len(crawled) != 2 {
		t.Errorf("expected 2 crawled, got %d", len(crawled))
	}
	if len(urlErrors) != 0 {
		t.Errorf("expected 0 url errors, got %d: %v", len(urlErrors), urlErrors)
	}
	if len(requestErrors) != 0 {
		t.Errorf("expected 0 request errors, got %d", len(requestErrors))
	}
}

func TestCheckURLStatus_StatusCodeClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		wantIsOk   bool
	}{
		{"200 OK", 200, true},
		{"201 Created", 201, true},
		{"299 edge of 2xx", 299, true},
		{"301 Moved Permanently", 301, false},
		{"404 Not Found", 404, false},
		{"500 Internal Server Error", 500, false},
		{"999 LinkedIn non-standard", 999, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer srv.Close()

			links := []Link{{originURL: "https://example.com/", url: srv.URL + "/"}}
			crawled, urlErrors, _ := checkURLStatus(links, 1, "HEAD", 5*time.Second)

			if len(crawled) != 1 {
				t.Fatalf("expected 1 crawled result, got %d", len(crawled))
			}
			if crawled[0].isOk != tt.wantIsOk {
				t.Errorf("status %d: isOk = %v, want %v", tt.statusCode, crawled[0].isOk, tt.wantIsOk)
			}
			if crawled[0].statusCode != tt.statusCode {
				t.Errorf("statusCode = %d, want %d", crawled[0].statusCode, tt.statusCode)
			}
			hasURLError := len(urlErrors) > 0
			if hasURLError == tt.wantIsOk {
				t.Errorf("status %d: urlErrors presence mismatch (hasURLError=%v, wantIsOk=%v)", tt.statusCode, hasURLError, tt.wantIsOk)
			}
		})
	}
}

func TestCheckURLStatus_UsesRequestMethod(t *testing.T) {
	t.Parallel()
	// mu guards receivedMethod which is written by the httptest server goroutine
	// and read by the test goroutine.
	var mu sync.Mutex
	var receivedMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedMethod = r.Method
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	links := []Link{{originURL: "https://example.com/", url: srv.URL + "/"}}

	t.Run("HEAD method", func(t *testing.T) {
		checkURLStatus(links, 1, "HEAD", 5*time.Second)
		mu.Lock()
		got := receivedMethod
		mu.Unlock()
		if got != http.MethodHead {
			t.Errorf("expected HEAD, got %s", got)
		}
	})
	t.Run("GET method", func(t *testing.T) {
		checkURLStatus(links, 1, "GET", 5*time.Second)
		mu.Lock()
		got := receivedMethod
		mu.Unlock()
		if got != http.MethodGet {
			t.Errorf("expected GET, got %s", got)
		}
	})
}

// TestCheckURLStatus_HeadFailsRetryWithGet verifies that when a HEAD request
// fails with a network error, the URL is retried with GET.
func TestCheckURLStatus_HeadFailsRetryWithGet(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	getCalled := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			// Abruptly close the connection to produce a network-level error.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("server does not support hijacking")
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		// GET succeeds normally.
		mu.Lock()
		getCalled++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	links := []Link{{originURL: "https://example.com/", url: srv.URL + "/"}}
	crawled, _, requestErrors := checkURLStatus(links, 1, "HEAD", 5*time.Second)

	mu.Lock()
	got := getCalled
	mu.Unlock()
	if got != 1 {
		t.Errorf("GET retry called %d times, want 1", got)
	}
	if len(requestErrors) != 0 {
		t.Errorf("expected 0 request errors after successful GET retry, got %d", len(requestErrors))
	}
	if len(crawled) != 1 || !crawled[0].isOk {
		t.Errorf("expected 1 successful crawled result, got %+v", crawled)
	}
}

// TestCheckURLStatus_BothMethodsFail verifies that when both HEAD and GET
// network requests fail, the URL ends up in requestErrors.
func TestCheckURLStatus_BothMethodsFail(t *testing.T) {
	t.Parallel()
	// Create a listener, grab its address, then close it so connections are refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not create listener: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	links := []Link{{originURL: "https://example.com/", url: "http://" + addr + "/"}}
	_, _, requestErrors := checkURLStatus(links, 1, "HEAD", 2*time.Second)

	if len(requestErrors) != 1 {
		t.Errorf("expected 1 request error, got %d", len(requestErrors))
	}
}

// TestCheckURLStatus_ConcurrencyLimit verifies the semaphore actually limits
// the number of in-flight requests to the configured concurrentLimit.
func TestCheckURLStatus_ConcurrencyLimit(t *testing.T) {
	t.Parallel()
	const limit = 3
	var (
		mu          sync.Mutex
		inflight    int
		maxInflight int
	)
	// Gate channel lets us control when server responses are released.
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		mu.Unlock()

		<-gate // hold until test releases

		mu.Lock()
		inflight--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var links []Link
	for i := range 10 {
		links = append(links, Link{url: fmt.Sprintf("%s/%d", srv.URL, i)})
	}

	done := make(chan struct{})
	go func() {
		checkURLStatus(links, limit, "HEAD", 5*time.Second)
		close(done)
	}()

	// Release all gates and wait for completion.
	for range links {
		gate <- struct{}{}
	}
	<-done

	mu.Lock()
	got := maxInflight
	mu.Unlock()
	if got > limit {
		t.Errorf("max concurrent requests = %d, want <= %d", got, limit)
	}
}

// ---- getXML -------------------------------------------------------------

func TestGetXML_Success(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != crawlerUserAgent {
			t.Errorf("unexpected User-Agent: %q", r.Header.Get("User-Agent"))
		}
		fmt.Fprint(w, `<urlset></urlset>`)
	}))
	defer srv.Close()

	resp, err := getXML(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestGetXML_NetworkError(t *testing.T) {
	t.Parallel()
	_, err := getXML("http://127.0.0.1:1/sitemap.xml", 2*time.Second)
	if err == nil {
		t.Error("expected error for unreachable server, got nil")
	}
}

// ---- getSitemap ---------------------------------------------------------

func TestGetSitemap_ParsesURLSet(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset>
  <url><loc>https://example.com/</loc></url>
  <url><loc>https://example.com/about</loc></url>
</urlset>`)
	}))
	defer srv.Close()

	pages, err := getSitemap(srv.URL+"/sitemap.xml", 5, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pages) != 2 {
		t.Errorf("expected 2 pages, got %d: %v", len(pages), pages)
	}
}

func TestGetSitemap_NetworkError(t *testing.T) {
	t.Parallel()
	_, err := getSitemap("http://127.0.0.1:1/sitemap.xml", 5, 2*time.Second)
	if err == nil {
		t.Error("expected error for unreachable server, got nil")
	}
}

func TestGetSitemap_InvalidResponse(t *testing.T) {
	t.Parallel()
	// Server closes the connection immediately without sending an HTTP response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	_, err := getSitemap(srv.URL+"/sitemap.xml", 5, 2*time.Second)
	if err == nil {
		t.Error("expected error when server closes connection, got nil")
	}
}

// ---- writeCSVReport -----------------------------------------------------

func TestWriteCSVReport_HeaderAndRows(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir() + "/report.csv"

	urlErrors := []CrawlResponse{
		{url: "https://example.com/broken", statusCode: 404, originText: "Click here", originURL: "https://example.com/"},
		{url: "https://example.com/gone", statusCode: 410, originText: "Old link", originURL: "https://example.com/page"},
	}
	reqErrors := []RequestError{
		{url: "https://example.com/timeout", err: fmt.Errorf("connection timeout"), originText: "Timeout link", originURL: "https://example.com/"},
	}

	if err := writeCSVReport(tmp, urlErrors, reqErrors); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	s := string(content)

	for _, want := range []string{
		"Broken URL", "HTTP Status Code", "Status Description", "Link Text", "Page Where Link Was Found",
		"https://example.com/broken", "404", "Not Found", "Click here",
		"https://example.com/gone", "410", "Gone",
		"https://example.com/timeout", "N/A", "connection timeout", "Timeout link",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("CSV missing expected value %q", want)
		}
	}
}

func TestWriteCSVReport_EmptyErrors_HeaderOnly(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir() + "/empty.csv"

	if err := writeCSVReport(tmp, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line (header only), got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "Broken URL") {
		t.Errorf("expected header row, got %q", lines[0])
	}
}

func TestWriteCSVReport_InvalidPath_ReturnsError(t *testing.T) {
	t.Parallel()
	err := writeCSVReport("/nonexistent/path/report.csv", nil, nil)
	if err == nil {
		t.Error("expected error for invalid path, got nil")
	}
}

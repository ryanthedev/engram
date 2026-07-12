package sources

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/html"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

var (
	allowLoopbackCrawl bool // Default false, set to true in tests
)

type webCrawlSource struct {
	seeds        []string
	maxPages     int
	maxPageBytes int
	maxFrontier  int
	delay        time.Duration
	userAgent    string
	deps         harvester.Deps

	// State/Cache
	robotsCache     map[string]*robotsRules
	lastRequestTime map[string]time.Time
	mu              sync.Mutex
}

var _ harvester.Source = (*webCrawlSource)(nil)

type robotsRules struct {
	disallowed []string
}

func (r *robotsRules) isAllowed(path string) bool {
	if path == "" {
		path = "/"
	}
	for _, prefix := range r.disallowed {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	return true
}

func init() {
	harvester.Register("web-crawl", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		var seeds []string
		if sVal, ok := cfg.Raw["seeds"]; ok {
			if slice, ok := sVal.([]any); ok {
				for _, elem := range slice {
					if str, ok := elem.(string); ok {
						seeds = append(seeds, str)
					}
				}
			} else if strSlice, ok := sVal.([]string); ok {
				seeds = strSlice
			}
		}
		if len(seeds) == 0 {
			return nil, fmt.Errorf("harvester: web-crawl: missing or empty required config 'seeds'")
		}

		maxPages := 100
		if mVal, ok := cfg.Raw["max_pages"]; ok {
			switch v := mVal.(type) {
			case int:
				maxPages = v
			case int64:
				maxPages = int(v)
			case float64:
				maxPages = int(v)
			}
		}
		if maxPages < 1 {
			return nil, fmt.Errorf("harvester: web-crawl: 'max_pages' must be at least 1, got %d", maxPages)
		}

		maxPageBytes := 1 << 20
		if mVal, ok := cfg.Raw["max_page_bytes"]; ok {
			switch v := mVal.(type) {
			case int:
				maxPageBytes = v
			case int64:
				maxPageBytes = int(v)
			case float64:
				maxPageBytes = int(v)
			}
		}
		if maxPageBytes < 1 {
			return nil, fmt.Errorf("harvester: web-crawl: 'max_page_bytes' must be at least 1, got %d", maxPageBytes)
		}

		delayStr := "200ms"
		if dVal, ok := cfg.Raw["delay"]; ok {
			if s, ok := dVal.(string); ok {
				delayStr = s
			}
		}
		delay, err := time.ParseDuration(delayStr)
		if err != nil {
			return nil, fmt.Errorf("harvester: web-crawl: invalid delay duration %q: %w", delayStr, err)
		}
		if delay < 0 {
			return nil, fmt.Errorf("harvester: web-crawl: 'delay' cannot be negative, got %s", delay)
		}

		maxFrontier := 10 * maxPages
		if mVal, ok := cfg.Raw["max_frontier"]; ok {
			switch v := mVal.(type) {
			case int:
				maxFrontier = v
			case int64:
				maxFrontier = int(v)
			case float64:
				maxFrontier = int(v)
			}
		}
		if maxFrontier < 1 {
			return nil, fmt.Errorf("harvester: web-crawl: 'max_frontier' must be at least 1, got %d", maxFrontier)
		}

		userAgent := "engram-harvester/1.0"
		if uVal, ok := cfg.Raw["user_agent"]; ok {
			if s, ok := uVal.(string); ok {
				userAgent = s
			}
		}

		return &webCrawlSource{
			seeds:           seeds,
			maxPages:        maxPages,
			maxPageBytes:    maxPageBytes,
			maxFrontier:     maxFrontier,
			delay:           delay,
			userAgent:       userAgent,
			deps:            deps,
			robotsCache:     make(map[string]*robotsRules),
			lastRequestTime: make(map[string]time.Time),
		}, nil
	})
}

// Type returns the source type name.
func (s *webCrawlSource) Type() string {
	return "web-crawl"
}

// Mode returns FullHarvest since crawl needs post-run sweeps.
func (s *webCrawlSource) Mode() harvester.HarvestMode {
	return harvester.FullHarvest
}

// Harvest executes the BFS crawl and outputs documents to the sink.
func (s *webCrawlSource) Harvest(ctx context.Context, sink harvester.Sink) error {
	// Parse and validate seeds up front
	parsedSeeds := make([]*url.URL, 0, len(s.seeds))
	for _, seedStr := range s.seeds {
		u, err := url.Parse(seedStr)
		if err != nil {
			return fmt.Errorf("harvester: web-crawl: invalid seed URL %q: %w", seedStr, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("harvester: web-crawl: invalid seed URL scheme %q: must be http or https", seedStr)
		}
		parsedSeeds = append(parsedSeeds, u)
	}

	enqueued := make(map[string]bool)
	type queueItem struct {
		u      *url.URL
		isSeed bool
	}
	var queue []queueItem

	seedLimitHit := false
	for _, u := range parsedSeeds {
		canon := canonicalizeURL(u)
		if !enqueued[canon] {
			if len(enqueued) >= s.maxFrontier {
				seedLimitHit = true
				break
			}
			enqueued[canon] = true
			queue = append(queue, queueItem{u: u, isSeed: true})
		}
	}
	if seedLimitHit {
		s.deps.Logger.WarnContext(ctx, "web-crawl: frontier limit reached during seed enqueueing",
			slog.Int("limit", s.maxFrontier),
		)
	}

	crawledCount := 0

	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				host = address
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("harvester: web-crawl: invalid IP address %q during dial", host)
			}
			if isBlockedIP(ip) {
				return fmt.Errorf("harvester: web-crawl: access to IP %s is blocked", ip)
			}
			return nil
		},
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("harvester: web-crawl: stopped after 10 redirects")
			}
			if len(via) > 0 {
				if req.URL.Hostname() != via[0].URL.Hostname() {
					return fmt.Errorf("harvester: web-crawl: redirect to different host %q blocked", req.URL.Host)
				}
			}
			return nil
		},
		Timeout: 15 * time.Second,
	}

	for len(queue) > 0 && crawledCount < s.maxPages {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("harvester: web-crawl: cancelled: %w", err)
		}

		item := queue[0]
		queue = queue[1:]

		// Politeness delay
		hostKey := item.u.Scheme + "://" + item.u.Host
		s.mu.Lock()
		lastTime := s.lastRequestTime[hostKey]
		elapsed := time.Since(lastTime)
		if elapsed < s.delay {
			waitDuration := s.delay - elapsed
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return fmt.Errorf("harvester: web-crawl: cancelled during politeness backoff: %w", ctx.Err())
			case <-time.After(waitDuration):
			}
		} else {
			s.mu.Unlock()
		}

		s.mu.Lock()
		s.lastRequestTime[hostKey] = time.Now()
		s.mu.Unlock()

		// Robots.txt check
		rules, _ := s.getRobotsRules(ctx, client, item.u)
		if !rules.isAllowed(item.u.RequestURI()) {
			s.deps.Logger.InfoContext(ctx, "web-crawl: skipping path disallowed by robots.txt",
				slog.String("url", item.u.String()),
			)
			continue
		}

		// Fetch page
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.u.String(), nil)
		if err != nil {
			if item.isSeed {
				return fmt.Errorf("harvester: web-crawl: failed to create request for seed URL %q: %w", item.u.String(), err)
			}
			s.deps.Logger.WarnContext(ctx, "web-crawl: failed to create request, skipping",
				slog.String("url", item.u.String()),
				slog.Any("error", err),
			)
			continue
		}
		req.Header.Set("User-Agent", s.userAgent)

		resp, err := client.Do(req)
		if err != nil {
			if item.isSeed {
				return fmt.Errorf("harvester: web-crawl: failed to fetch seed URL %q: %w", item.u.String(), err)
			}
			s.deps.Logger.WarnContext(ctx, "web-crawl: failed to fetch URL, skipping",
				slog.String("url", item.u.String()),
				slog.Any("error", err),
			)
			continue
		}

		// Check status code
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if item.isSeed {
				return fmt.Errorf("harvester: web-crawl: seed URL returned status %d", resp.StatusCode)
			}
			s.deps.Logger.WarnContext(ctx, "web-crawl: URL returned bad status, skipping",
				slog.String("url", item.u.String()),
				slog.Int("status", resp.StatusCode),
			)
			continue
		}

		// Check content-type
		contentType := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/html") {
			resp.Body.Close()
			if item.isSeed {
				return fmt.Errorf("harvester: web-crawl: seed URL is not text/html, got %q", contentType)
			}
			s.deps.Logger.InfoContext(ctx, "web-crawl: skipping non-text/html page",
				slog.String("url", item.u.String()),
				slog.String("content_type", contentType),
			)
			continue
		}

		// Read and limit body (use maxPageBytes + 1 to detect truncation)
		limitReader := io.LimitReader(resp.Body, int64(s.maxPageBytes)+1)
		bodyBytes, err := io.ReadAll(limitReader)
		resp.Body.Close()
		if err != nil {
			if item.isSeed {
				return fmt.Errorf("harvester: web-crawl: failed to read seed URL body %q: %w", item.u.String(), err)
			}
			s.deps.Logger.WarnContext(ctx, "web-crawl: failed to read page body, skipping",
				slog.String("url", item.u.String()),
				slog.Any("error", err),
			)
			continue
		}

		var pageContent []byte
		if int64(len(bodyBytes)) > int64(s.maxPageBytes) {
			s.deps.Logger.WarnContext(ctx, "web-crawl: page body truncated because it exceeded max_page_bytes",
				slog.String("url", item.u.String()),
				slog.Int("limit", s.maxPageBytes),
			)
			pageContent = bodyBytes[:s.maxPageBytes]
		} else {
			pageContent = bodyBytes
		}

		finalURL := resp.Request.URL
		if finalURL == nil {
			finalURL = item.u
		}

		finalCanon := canonicalizeURL(finalURL)
		if !enqueued[finalCanon] {
			if len(enqueued) < s.maxFrontier {
				enqueued[finalCanon] = true
			}
		}

		title, text, links, err := parseHTML(bytes.NewReader(pageContent), finalURL)
		if err != nil {
			if item.isSeed {
				return fmt.Errorf("harvester: web-crawl: failed to parse HTML of seed URL %q: %w", item.u.String(), err)
			}
			s.deps.Logger.WarnContext(ctx, "web-crawl: failed to parse HTML, skipping",
				slog.String("url", item.u.String()),
				slog.Any("error", err),
			)
			continue
		}

		doc := mcp.KnowledgeDoc{
			ID:            finalCanon,
			Title:         title,
			Text:          text,
			SourceVersion: "crawl:" + time.Now().Format(time.RFC3339),
			Fields: map[string]any{
				"url": finalCanon,
			},
		}

		if err := sink.Add(doc); err != nil {
			return fmt.Errorf("harvester: web-crawl: failed to add doc to sink: %w", err)
		}
		crawledCount++

		// Enqueue links
		frontierLimitHit := false
		for _, link := range links {
			parsedLink, err := url.Parse(link)
			if err != nil {
				continue
			}
			linkCanon := canonicalizeURL(parsedLink)
			if !enqueued[linkCanon] {
				if len(enqueued) >= s.maxFrontier {
					frontierLimitHit = true
					break
				}
				enqueued[linkCanon] = true
				queue = append(queue, queueItem{u: parsedLink, isSeed: false})
			}
		}
		if frontierLimitHit {
			s.deps.Logger.WarnContext(ctx, "web-crawl: frontier limit reached, stopping discovery",
				slog.Int("limit", s.maxFrontier),
			)
		}
	}

	return nil
}

func (s *webCrawlSource) getRobotsRules(ctx context.Context, client *http.Client, pageURL *url.URL) (*robotsRules, error) {
	hostKey := pageURL.Scheme + "://" + pageURL.Host
	s.mu.Lock()
	rules, exists := s.robotsCache[hostKey]
	s.mu.Unlock()
	if exists {
		return rules, nil
	}

	robotsURL := hostKey + "/robots.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating robots.txt request: %w", err)
	}
	req.Header.Set("User-Agent", s.userAgent)

	resp, err := client.Do(req)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "web-crawl: failed to fetch robots.txt, allowing all",
			slog.String("url", robotsURL),
			slog.Any("error", err),
		)
		emptyRules := &robotsRules{}
		s.mu.Lock()
		s.robotsCache[hostKey] = emptyRules
		s.mu.Unlock()
		return emptyRules, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		emptyRules := &robotsRules{}
		s.mu.Lock()
		s.robotsCache[hostKey] = emptyRules
		s.mu.Unlock()
		return emptyRules, nil
	}

	limitReader := io.LimitReader(resp.Body, int64(s.maxPageBytes))
	bodyBytes, err := io.ReadAll(limitReader)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "web-crawl: failed to read robots.txt, allowing all",
			slog.String("url", robotsURL),
			slog.Any("error", err),
		)
		emptyRules := &robotsRules{}
		s.mu.Lock()
		s.robotsCache[hostKey] = emptyRules
		s.mu.Unlock()
		return emptyRules, nil
	}

	parsedRules := parseRobotsTxt(string(bodyBytes), s.userAgent)
	s.mu.Lock()
	s.robotsCache[hostKey] = parsedRules
	s.mu.Unlock()
	return parsedRules, nil
}

func parseRobotsTxt(body string, myUA string) *robotsRules {
	lines := strings.Split(body, "\n")
	var defaultDisallows []string
	var myUADisallows []string
	var currentUAs []string

	for _, line := range lines {
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		if key == "user-agent" {
			ua := strings.ToLower(val)
			currentUAs = append(currentUAs, ua)
		} else if key == "disallow" {
			for _, ua := range currentUAs {
				if ua == "*" {
					defaultDisallows = append(defaultDisallows, val)
				}
				if myUA != "" && strings.Contains(strings.ToLower(myUA), ua) {
					myUADisallows = append(myUADisallows, val)
				}
			}
		}
	}

	rules := &robotsRules{}
	if len(myUADisallows) > 0 {
		rules.disallowed = myUADisallows
	} else {
		rules.disallowed = defaultDisallows
	}
	return rules
}

func parseHTML(body io.Reader, pageURL *url.URL) (title string, text string, links []string, err error) {
	doc, err := html.Parse(body)
	if err != nil {
		return "", "", nil, fmt.Errorf("parsing html: %w", err)
	}

	var titleBuilder strings.Builder
	var textBuilder strings.Builder
	var linksList []string

	var traverse func(*html.Node, bool)
	traverse = func(n *html.Node, inTitle bool) {
		if n.Type == html.ElementNode {
			name := strings.ToLower(n.Data)
			if name == "script" || name == "style" {
				return
			}
			if name == "title" {
				inTitle = true
			}
			if name == "a" {
				for _, attr := range n.Attr {
					if strings.ToLower(attr.Key) == "href" {
						val := strings.TrimSpace(attr.Val)
						if val != "" {
							resolved, err := pageURL.Parse(val)
							if err == nil {
								scheme := strings.ToLower(resolved.Scheme)
								if scheme == "http" || scheme == "https" {
									if resolved.Host == pageURL.Host {
										linksList = append(linksList, resolved.String())
									}
								}
							}
						}
					}
				}
			}
		} else if n.Type == html.TextNode {
			data := n.Data
			if inTitle {
				titleBuilder.WriteString(data)
			} else {
				textBuilder.WriteString(data)
				textBuilder.WriteByte(' ')
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverse(c, inTitle)
		}
	}

	traverse(doc, false)

	title = collapseWhitespace(titleBuilder.String())
	text = collapseWhitespace(textBuilder.String())

	return title, text, linksList, nil
}

func collapseWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func canonicalizeURL(u *url.URL) string {
	uCopy := *u
	uCopy.Fragment = ""
	uCopy.Scheme = strings.ToLower(uCopy.Scheme)
	uCopy.Host = strings.ToLower(uCopy.Host)

	host, port, err := net.SplitHostPort(uCopy.Host)
	if err == nil {
		if (uCopy.Scheme == "http" && port == "80") || (uCopy.Scheme == "https" && port == "443") {
			uCopy.Host = host
		}
	}

	return uCopy.String()
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	if ip.IsUnspecified() {
		return true
	}

	if ip.IsMulticast() {
		return true
	}

	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 127 {
			return !allowLoopbackCrawl
		}

		if ip4[0] == 10 {
			return true
		}
		if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return true
		}
		if ip4[0] == 192 && ip4[1] == 168 {
			return true
		}

		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}

		return false
	}

	if ip.Equal(net.IPv6loopback) {
		return !allowLoopbackCrawl
	}

	if len(ip) == 16 && (ip[0]&0xfe) == 0xfc {
		return true
	}

	if len(ip) == 16 && ip[0] == 0xfe && (ip[1]&0xc0) == 0x80 {
		return true
	}

	return false
}

package tracker

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/berejant/movie-torrent-finder/internal/media"
)

var (
	reSynopsis      = regexp.MustCompile(`(?i)^\s*(?:сюжет|опис|синопсис|описание|synopsis|description|plot)\s*[:：\-–—]`)
	reCompanyPrefix = regexp.MustCompile(`(?i)^\s*(?:кінокомпані[яії]|кинокомпани[яи]|студі[яії]|студи[яи]|виробництво|производство|виробник|production(?:\s+compan(?:y|ies))?|compan(?:y|ies)|studios?|distributor|дистриб[’'ʼ]?ютор|випущено|выпущено)\s*[:：\-–—]\s*(.+)$`)
)

// Seeder and leecher columns are parsed by nobody on purpose: results are
// frequently cross-posted with stale swarm numbers, so they are not a signal.
// The size column is configurable via TrackerOptions.SizeCellIndex.

// parseResults extracts every usable row from a search result page.
func (c *Client) parseResults(doc *goquery.Document, pageURL string) []Torrent {
	base, err := url.Parse(pageURL)
	if err != nil {
		base = c.baseURL
	}

	var torrents []Torrent

	doc.Find(c.options.ResultRowSelector).Each(func(_ int, row *goquery.Selection) {
		torrent, ok := c.parseRow(row, base)
		if !ok {
			return
		}
		if c.cfg.MaxSizeBytes > 0 && torrent.SizeBytes > c.cfg.MaxSizeBytes {
			return
		}
		torrents = append(torrents, torrent)
	})

	return torrents
}

// parseRow reads one result row. A row without both a topic link and a
// download link is not a result (headers, separators, pagination), so it is
// skipped silently.
func (c *Client) parseRow(row *goquery.Selection, base *url.URL) (Torrent, bool) {
	topic := row.Find(c.options.TopicLinkSelector).First()
	download := row.Find(c.options.DownloadLinkSelector).First()
	if topic.Length() == 0 || download.Length() == 0 {
		return Torrent{}, false
	}

	title := strings.TrimSpace(topic.Text())
	downloadHref := strings.TrimSpace(download.AttrOr("href", ""))
	if title == "" || downloadHref == "" {
		return Torrent{}, false
	}

	forum := ""
	if link := row.Find(c.options.ForumLinkSelector).First(); link.Length() > 0 {
		forum = strings.TrimSpace(link.Text())
	}

	sizeText := c.findSizeText(row)

	return Torrent{
		Title:       title,
		Forum:       forum,
		TopicURL:    absoluteURL(base, strings.TrimSpace(topic.AttrOr("href", ""))),
		DownloadURL: absoluteURL(base, downloadHref),
		SizeText:    sizeText,
		Attributes:  media.Analyze(title, sizeText),
	}, true
}

// findSizeText prefers the configured column but falls back to scanning the
// row, so a theme that shifts columns degrades instead of losing every size.
func (c *Client) findSizeText(row *goquery.Selection) string {
	cells := row.Find("td")

	if index := c.options.SizeCellIndex; index >= 0 && cells.Length() > index {
		candidate := strings.TrimSpace(cells.Eq(index).Text())
		if media.ParseSizeBytes(candidate) > 0 {
			return candidate
		}
	}

	var found string
	cells.EachWithBreak(func(_ int, cell *goquery.Selection) bool {
		text := strings.TrimSpace(cell.Text())
		if media.ParseSizeBytes(text) > 0 {
			found = text
			return false
		}
		return true
	})
	return found
}

func absoluteURL(base *url.URL, href string) string {
	if href == "" {
		return ""
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base == nil {
		return parsed.String()
	}
	return base.ResolveReference(parsed).String()
}

// TopicDetails contains metadata extracted from a topic page.
type TopicDetails struct {
	Title    string
	Company  string
	Metadata string
	FullText string
}

// ParseTopicDetails parses the initial release post of a topic page to extract
// metadata such as production company/studio and header info before synopsis.
func (c *Client) ParseTopicDetails(doc *goquery.Document) TopicDetails {
	if doc == nil {
		return TopicDetails{}
	}

	selector := c.options.TopicBodySelector
	if selector == "" {
		selector = "span.postbody, div.post_body, .postbody, .post_body, td.message, div.message"
	}

	firstPost := doc.Find(selector).First()
	if firstPost.Length() == 0 {
		return TopicDetails{}
	}

	firstPost.Find("br").ReplaceWithHtml("\n")

	rawText := firstPost.Text()
	lines := strings.Split(rawText, "\n")

	var (
		companies []string
		metaLines []string
		allLines  []string
		inMeta    = true
	)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		allLines = append(allLines, trimmed)

		if reSynopsis.MatchString(trimmed) {
			inMeta = false
		}
		if inMeta {
			metaLines = append(metaLines, trimmed)
		}

		if match := reCompanyPrefix.FindStringSubmatch(trimmed); len(match) == 2 {
			company := strings.TrimSpace(match[1])
			if company != "" {
				companies = append(companies, company)
			}
		}
	}

	return TopicDetails{
		Company:  strings.Join(companies, ", "),
		Metadata: strings.Join(metaLines, "\n"),
		FullText: strings.Join(allLines, "\n"),
	}
}

package tracker

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/berejant/movie-torrent-finder/internal/config"
	"github.com/berejant/movie-torrent-finder/internal/media"
)

// The fixtures are pages saved from the live trackers, kept in the repository
// so a markup change can be diagnosed against the same input the parser saw.
const fixtureDir = "../../html-examples"

func loadFixture(t *testing.T, name string) *goquery.Document {
	t.Helper()

	file, err := os.Open(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = file.Close() }()

	doc, err := goquery.NewDocumentFromReader(file)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return doc
}

func presetClient(t *testing.T, preset, name, baseURL string) *Client {
	t.Helper()

	profile, ok := config.LookupPreset(preset)
	if !ok {
		t.Fatalf("preset %q is not registered", preset)
	}

	client, err := New(config.Tracker{
		Name:           name,
		BaseURL:        baseURL,
		Login:          "tester",
		Password:       "secret",
		Priority:       1,
		TimeoutSeconds: 5,
		RPS:            50,
		UserAgent:      config.DefaultUserAgent,
		Options:        profile.Options,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return client
}

// TestParseTolokaSearchResults runs the toloka preset against a real saved
// search page. toloka renders a phpBB2 table rather than TorrentPier's, so this
// is the check that the preset — not the parser — carries the difference.
func TestParseTolokaSearchResults(t *testing.T) {
	client := presetClient(t, "toloka", "toloka", "https://toloka.to")
	doc := loadFixture(t, "toloka/search-results.html")

	torrents := client.parseResults(doc, "https://toloka.to/tracker.php?nm=%D0%97%D0%B0%D0%B3%D1%83%D0%B1%D0%BB%D0%B5%D0%BD%D0%B0")

	const wantRows = 65
	if len(torrents) != wantRows {
		t.Fatalf("parsed %d rows, want %d", len(torrents), wantRows)
	}

	first := torrents[0]
	if want := "Зникла кімната / Загублена кімната (Сезон 1) / The Lost Room (Season 1) (2006) WEB-DL 1080p Ukr/Eng | sub Eng"; first.Title != want {
		t.Errorf("title = %q, want %q", first.Title, want)
	}
	if want := "Серіали в HD"; first.Forum != want {
		t.Errorf("forum = %q, want %q", first.Forum, want)
	}
	// Relative hrefs on toloka carry no leading slash ("download.php?id=…"),
	// so they only resolve correctly against the search page URL.
	if want := "https://toloka.to/download.php?id=712129&sid=d4db50f596e1ac126c61017bfc04b532"; first.DownloadURL != want {
		t.Errorf("download url = %q, want %q", first.DownloadURL, want)
	}
	if want := "https://toloka.to/t697279?sid=d4db50f596e1ac126c61017bfc04b532"; first.TopicURL != want {
		t.Errorf("topic url = %q, want %q", first.TopicURL, want)
	}
	if want := media.ParseSizeBytes("23.44 GB"); first.SizeBytes != want {
		t.Errorf("size = %d bytes (%q), want %d", first.SizeBytes, first.SizeText, want)
	}
	if first.Quality != media.Quality1080 {
		t.Errorf("quality = %q, want %q", first.Quality, media.Quality1080)
	}

	// The size column is the one most likely to drift: a shifted index still
	// parses because of the row scan, so assert every row produced a size.
	for _, torrent := range torrents {
		if torrent.SizeBytes <= 0 {
			t.Errorf("row %q has no parseable size (cell text %q)", torrent.Title, torrent.SizeText)
		}
		if !strings.HasPrefix(torrent.DownloadURL, "https://toloka.to/download.php?id=") {
			t.Errorf("row %q has download url %q", torrent.Title, torrent.DownloadURL)
		}
	}
}

// TestTolokaSessionSelectors covers the pair of selectors that decide whether a
// re-login is needed. They are easy to get wrong here: toloka's logged-in
// header links /login.php?logout=true, so a login-link selector would report
// every authenticated page as logged out.
func TestTolokaSessionSelectors(t *testing.T) {
	client := presetClient(t, "toloka", "toloka", "https://toloka.to")

	loggedIn := loadFixture(t, "toloka/search-results.html")
	if client.looksLoggedOut(loggedIn) {
		t.Error("an authenticated search page must not look logged out")
	}

	loggedOut := loadFixture(t, "toloka/logout-search-results.html")
	if !client.looksLoggedOut(loggedOut) {
		t.Error("an anonymous search page must look logged out")
	}
	// An anonymous visitor gets the table chrome but no rows at all, which is
	// why toloka cannot be searched without a session.
	if torrents := client.parseResults(loggedOut, "https://toloka.to/tracker.php"); len(torrents) != 0 {
		t.Errorf("anonymous page yielded %d rows, want 0", len(torrents))
	}
}

func TestExtractTolokaTopicDetails(t *testing.T) {
	client := presetClient(t, "toloka", "toloka", "https://toloka.to")

	docOdyssey := loadFixture(t, "toloka/t699884.html")
	odysseyDetails := client.ParseTopicDetails(docOdyssey)
	if odysseyDetails.Company != "The Asylum" {
		t.Errorf("Odyssey company = %q, want 'The Asylum'", odysseyDetails.Company)
	}
	if !strings.Contains(odysseyDetails.Metadata, "Країна: США") {
		t.Errorf("Odyssey metadata missing country: %q", odysseyDetails.Metadata)
	}
	if strings.Contains(odysseyDetails.Metadata, "Після десяти років кривавої") {
		t.Error("Odyssey metadata should not contain the synopsis")
	}

	docGoneGirl := loadFixture(t, "toloka/torrent-page.html")
	goneGirlDetails := client.ParseTopicDetails(docGoneGirl)
	wantGoneGirlCompany := "Artemple - Hollywood, New Regency Pictures, Pacific Standard, Regency Enterprises"
	if goneGirlDetails.Company != wantGoneGirlCompany {
		t.Errorf("Gone Girl company = %q, want %q", goneGirlDetails.Company, wantGoneGirlCompany)
	}
	if isMock, _ := media.IsMockbuster("The Odyssey (2026)", odysseyDetails.Company, odysseyDetails.Metadata, nil); !isMock {
		t.Error("Odyssey (2026) must be identified as mockbuster")
	}
	if isMock, _ := media.IsMockbuster("Gone Girl (2014)", goneGirlDetails.Company, goneGirlDetails.Metadata, nil); isMock {
		t.Error("Gone Girl (2014) must not be identified as mockbuster")
	}
}


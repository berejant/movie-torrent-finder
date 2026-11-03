package media

import (
	"regexp"
	"strings"
)

// DefaultMockbusterCompanies is the default list of low-budget mockbuster and
// B-movie studios whose cash-in releases should be filtered out.
var DefaultMockbusterCompanies = []string{
	"The Asylum",
	"Full Moon Features",
	"Full Moon Pictures",
	"Full Moon Entertainment",
	"Jagged Edge Productions",
	"ITN Studios",
	"ITN Distribution",
	"ITN Films",
	"Video Brinquedo",
	"Vídeo Brinquedo",
	"Dingo Pictures",
}

// IsTitleMockbuster checks if the release title itself contains a mockbuster studio tag.
func IsTitleMockbuster(title string, blockedCompanies []string) (bool, string) {
	if len(blockedCompanies) == 0 {
		blockedCompanies = DefaultMockbusterCompanies
	}

	for _, company := range blockedCompanies {
		cleaned := strings.TrimSpace(company)
		if cleaned == "" {
			continue
		}
		if matchCompanyPhrase(title, cleaned) {
			return true, cleaned
		}
	}
	return false, ""
}

// IsMockbuster determines if a release is a mockbuster based on its title,
// parsed production company field, and topic metadata.
func IsMockbuster(title, company, metadata string, blockedCompanies []string) (bool, string) {
	if len(blockedCompanies) == 0 {
		blockedCompanies = DefaultMockbusterCompanies
	}

	// 1. Fast check against the title
	if isMock, matched := IsTitleMockbuster(title, blockedCompanies); isMock {
		return true, matched
	}

	// 2. Check the explicit company/studio field from topic
	if company != "" {
		for _, bc := range blockedCompanies {
			cleaned := strings.TrimSpace(bc)
			if cleaned == "" {
				continue
			}
			if matchCompanyPhrase(company, cleaned) {
				return true, cleaned
			}
		}

		// Also handle common studio short names inside the company field:
		// e.g. "Asylum" for "The Asylum", "Full Moon", "Jagged Edge".
		shortNames := map[string]string{
			"asylum":      "The Asylum",
			"full moon":   "Full Moon Features",
			"jagged edge": "Jagged Edge Productions",
		}
		for short, full := range shortNames {
			if matchCompanyPhrase(company, short) {
				return true, full
			}
		}
	}

	// 3. Check metadata (the release info block before synopsis)
	if metadata != "" {
		for _, bc := range blockedCompanies {
			cleaned := strings.TrimSpace(bc)
			if cleaned == "" {
				continue
			}
			if matchCompanyPhrase(metadata, cleaned) {
				return true, cleaned
			}
		}
	}

	return false, ""
}

// matchCompanyPhrase checks whether target contains company as a distinct phrase.
func matchCompanyPhrase(target, company string) bool {
	targetNorm := normalizeSpaces(target)
	companyNorm := normalizeSpaces(company)
	if targetNorm == "" || companyNorm == "" {
		return false
	}

	// Create regex with word boundaries around the phrase.
	pattern := `(?i)(?:^|[^\p{L}\p{N}])` + regexp.QuoteMeta(companyNorm) + `(?:$|[^\p{L}\p{N}])`
	re, err := regexp.Compile(pattern)
	if err != nil {
		return strings.Contains(strings.ToLower(targetNorm), strings.ToLower(companyNorm))
	}
	return re.MatchString(targetNorm)
}

func normalizeSpaces(s string) string {
	return strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
}

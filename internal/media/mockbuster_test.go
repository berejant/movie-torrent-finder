package media

import (
	"testing"
)

func TestIsTitleMockbuster(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		blocked []string
		want    bool
		matched string
	}{
		{
			name:    "clean title",
			title:   "The Odyssey (2026) 1080p WEB-DL",
			want:    false,
			matched: "",
		},
		{
			name:    "title with The Asylum in brackets",
			title:   "The Odyssey (2026) [The Asylum] 1080p",
			want:    true,
			matched: "The Asylum",
		},
		{
			name:    "title with Full Moon Features",
			title:   "Puppet Master (1989) (Full Moon Features) 720p",
			want:    true,
			matched: "Full Moon Features",
		},
		{
			name:    "title with Jagged Edge Productions",
			title:   "Winnie the Pooh: Blood and Honey (2023) [Jagged Edge Productions]",
			want:    true,
			matched: "Jagged Edge Productions",
		},
		{
			name:    "title with ITN Studios",
			title:   "Alien Predator (2020) ITN Studios 1080p",
			want:    true,
			matched: "ITN Studios",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, matched := IsTitleMockbuster(tc.title, tc.blocked)
			if got != tc.want {
				t.Errorf("IsTitleMockbuster() = %v, want %v", got, tc.want)
			}
			if matched != tc.matched {
				t.Errorf("matched = %q, want %q", matched, tc.matched)
			}
		})
	}
}

func TestIsMockbuster(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		company  string
		metadata string
		blocked  []string
		want     bool
		matched  string
	}{
		{
			name:     "Toloka Odyssey 2026 Asylum mockbuster",
			title:    "Одіссея / The Odyssey (2026) WEB-DL 1080p Eng | sub Ukr/Eng",
			company:  "The Asylum",
			metadata: "Країна: США\nКінокомпанія: The Asylum\nРежисер: Марсель Вальц (Marcel Walz)",
			want:     true,
			matched:  "The Asylum",
		},
		{
			name:     "Company field short name Asylum",
			title:    "Titanic II (2010)",
			company:  "Asylum",
			metadata: "Кінокомпанія: Asylum",
			want:     true,
			matched:  "The Asylum",
		},
		{
			name:     "Legitimate Gone Girl release",
			title:    "Загублена / Gone Girl (2014) WEB-DL 4K 2160p H.265 Ukr/Eng | Sub Ukr/Eng",
			company:  "Artemple - Hollywood, New Regency Pictures, Pacific Standard, Regency Enterprises",
			metadata: "Країна: США\nКінокомпанія: Artemple - Hollywood, New Regency Pictures, Pacific Standard, Regency Enterprises\nРежисер: Девід Фінчер",
			want:     false,
			matched:  "",
		},
		{
			name:     "Legitimate Shutter Island release",
			title:    "Острів проклятих / Shutter Island (2010) UHD BDRemux 2160p 4K HDR H.265 4xUkr/Eng | Sub Eng",
			company:  "Paramount Pictures, Phoenix Pictures, Sikelia Productions, Appian Way",
			metadata: "Країна: США\nКінокомпанія: Paramount Pictures, Phoenix Pictures, Sikelia Productions, Appian Way\nРежисер: Мартін Скорсезе",
			want:     false,
			matched:  "",
		},
		{
			name:     "Full Moon Features in company list",
			title:    "Subspecies (1991)",
			company:  "Full Moon Features, Castel Film Romania",
			metadata: "Кінокомпанія: Full Moon Features",
			want:     true,
			matched:  "Full Moon Features",
		},
		{
			name:     "ITN Distribution in metadata",
			title:    "The Curse of Humpty Dumpty (2021)",
			company:  "",
			metadata: "Production: ITN Distribution\nDirector: Scott Jeffrey",
			want:     true,
			matched:  "ITN Distribution",
		},
		{
			name:     "Custom blocked company list",
			title:    "Custom B-Movie",
			company:  "Bad Movie Corp",
			metadata: "",
			blocked:  []string{"Bad Movie Corp"},
			want:     true,
			matched:  "Bad Movie Corp",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, matched := IsMockbuster(tc.title, tc.company, tc.metadata, tc.blocked)
			if got != tc.want {
				t.Errorf("IsMockbuster() = %v, want %v", got, tc.want)
			}
			if matched != tc.matched {
				t.Errorf("matched = %q, want %q", matched, tc.matched)
			}
		})
	}
}

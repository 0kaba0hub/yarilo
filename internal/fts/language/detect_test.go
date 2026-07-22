package language

import "testing"

func TestDetectLanguage(t *testing.T) {
	// Long enough, clearly-distinguishing sentences for a trigram-based
	// classifier to have a stable, reliable answer.
	const englishSample = "The quick brown fox jumps over the lazy dog while the sun shines brightly over the green meadow this morning."
	const germanSample = "Der schnelle braune Fuchs springt über den faulen Hund während die Sonne hell über die grüne Wiese scheint heute Morgen."
	const frenchSample = "Le renard brun rapide saute par-dessus le chien paresseux pendant que le soleil brille sur le pré vert ce matin."

	tests := []struct {
		name       string
		sample     string
		candidates []string
		wantLang   string
		wantOK     bool
	}{
		{"english recognized", englishSample, []string{"en", "de", "fr"}, "en", true},
		{"german recognized", germanSample, []string{"en", "de", "fr"}, "de", true},
		{"french recognized", frenchSample, []string{"en", "de", "fr"}, "fr", true},
		{"too short", "hi", []string{"en", "de", "fr"}, "", false},
		{"empty", "", []string{"en", "de", "fr"}, "", false},
		{"no candidates", englishSample, nil, "", false},
		{
			// German text, but German isn't in the candidate set — detection
			// must not return a language the caller didn't offer, mirroring
			// the reference implementation's language_match_lists restriction.
			name:       "detected language outside candidate set",
			sample:     germanSample,
			candidates: []string{"en", "fr"},
			wantLang:   "",
			wantOK:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lang, ok := detectLanguage(tc.sample, tc.candidates, 0)
			if ok != tc.wantOK {
				t.Fatalf("detectLanguage() ok = %v, want %v (lang=%q)", ok, tc.wantOK, lang)
			}
			if ok && lang != tc.wantLang {
				t.Errorf("detectLanguage() = %q, want %q", lang, tc.wantLang)
			}
		})
	}
}

// TestDetectLanguageUkrainianVsRussian (#718) proves uk is detectable at
// all (isoToWhatlang) and distinguished from its closest neighbour ru —
// exactly what keeps a Ukrainian part in a mixed uk/ru mailbox from being
// mis-stemmed under the ru chain.
func TestDetectLanguageUkrainianVsRussian(t *testing.T) {
	const ukrainianSample = "Доброго дня! Сьогодні чудова погода, і я хочу піти погуляти у парку разом із друзями та випити смачної кави."
	const russianSample = "Быстрая коричневая лиса прыгает через ленивую собаку, пока солнце ярко светит над зелёной поляной сегодня утром."

	tests := []struct {
		name     string
		sample   string
		wantLang string
	}{
		{"ukrainian recognized", ukrainianSample, "uk"},
		{"russian recognized", russianSample, "ru"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lang, ok := detectLanguage(tc.sample, []string{"uk", "ru"}, 0)
			if !ok {
				t.Fatalf("detectLanguage() ok = false, want true")
			}
			if lang != tc.wantLang {
				t.Errorf("detectLanguage() = %q, want %q", lang, tc.wantLang)
			}
		})
	}
}

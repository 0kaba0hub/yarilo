package language

import "testing"

func TestDetectLanguage(t *testing.T) {
	// Long enough for a trigram classifier to give a stable answer.
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
			// Detection must not return a language outside the candidate set.
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

// uk must be detectable and distinguished from ru, so uk parts in a
// mixed mailbox aren't stemmed under the ru chain.
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

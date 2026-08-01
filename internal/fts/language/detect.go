package language

import "github.com/abadojack/whatlanggo"

// defaultMinDetectSample: below this many runes the trigram model isn't
// reliable; don't even call it.
const defaultMinDetectSample = 10

// detectLanguage restricts detection to candidates. minRunes overrides
// defaultMinDetectSample when > 0. ok=false when the sample is too short
// or the result unreliable; callers fall back to the first configured
// language.
func detectLanguage(sample string, candidates []string, minRunes int) (lang string, ok bool) {
	if minRunes <= 0 {
		minRunes = defaultMinDetectSample
	}
	if len([]rune(sample)) < minRunes {
		return "", false
	}
	whitelist := make(map[whatlanggo.Lang]bool, len(candidates))
	for _, c := range candidates {
		if wl, found := isoToWhatlang[c]; found {
			whitelist[wl] = true
		}
	}
	if len(whitelist) == 0 {
		return "", false
	}
	info := whatlanggo.DetectWithOptions(sample, whatlanggo.Options{Whitelist: whitelist})
	if !info.IsReliable() {
		return "", false
	}
	code := info.Lang.Iso6391()
	for _, c := range candidates {
		if c == code {
			return code, true
		}
	}
	return "", false
}

// isoToWhatlang maps the ISO 639-1 codes this package can build a filter
// chain for. uk has no Snowball stemmer but is still detectable
// (lowercase+stopwords chain), which keeps uk parts from being mis-stemmed
// under ru in a mixed mailbox.
var isoToWhatlang = map[string]whatlanggo.Lang{
	"en": whatlanggo.Eng,
	"fr": whatlanggo.Fra,
	"de": whatlanggo.Deu,
	"it": whatlanggo.Ita,
	"pt": whatlanggo.Por,
	"ru": whatlanggo.Rus,
	"es": whatlanggo.Spa,
	"uk": whatlanggo.Ukr,
}

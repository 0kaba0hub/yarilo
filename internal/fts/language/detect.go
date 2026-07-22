package language

import "github.com/abadojack/whatlanggo"

// defaultMinDetectSample is the minimum sample length (in runes) below which
// detection is considered unreliable, mirroring the reference implementation's
// SHORT/UNKNOWN fallback (per-part language detection falls back to the
// first configured language when there isn't enough text to classify).
// whatlanggo's own trigram model needs a comparable amount of text to
// produce a stable result; below this we don't even bother calling it. Used
// when MultiChain wasn't given an explicit override (see NewMultiChain).
const defaultMinDetectSample = 10

// detectLanguage restricts detection to candidates (mirroring the reference
// implementation's language_match_lists — Norwegian bokmål/nynorsk etc. both
// collapse to their base ISO 639-1 code the same way). minRunes overrides
// defaultMinDetectSample when > 0 (#696: tunable detection threshold).
// Returns ok=false when the sample is too short or the result isn't
// reliable enough to trust — callers must fall back to the first configured
// language, never guess.
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
// chain for (filter.go) to whatlanggo's Lang enum. Detection is only ever
// restricted to (and can only ever return) a language this package can
// actually configure — there is no point detecting a language with no
// filter chain to apply. That no longer means "can stem": uk (#718) has no
// Snowball algorithm at all (passthroughFilter stands in) but is still
// detectable, since lowercase+stopwords is a real, distinct filter chain —
// and detecting it is exactly what keeps a uk part from being mis-stemmed
// under a neighbouring language's chain (e.g. ru) in a mixed-language
// mailbox.
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

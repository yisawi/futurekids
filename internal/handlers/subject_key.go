package handlers

import (
	"strings"
	"unicode"
)

// SubjectKeys are the values subject_key can take, in the order the keyword table checks them,
// followed by the fallback "other".
var SubjectKeys = []string{"math", "pe", "arabic", "english", "islamic", "computer", "science", "social", "art", "music", "other"}

// subjectKeywords is checked in order and the first match wins. Arabic keywords are matched
// anywhere in the normalized name, Latin keywords only as whole words. "math" comes before "pe"
// so رياضيات never reads as رياضة, and "computer" before "science" because علوم is inside معلوماتيه.
var subjectKeywords = []struct {
	key   string
	words []string
}{
	{"math", []string{"رياضيات", "حساب", "math", "maths", "mathematics"}},
	{"pe", []string{"رياضه", "رياضيه", "بدنيه", "physical", "sport", "sports"}},
	{"arabic", []string{"عربي", "قراءه", "arabic"}},
	{"english", []string{"انجليز", "انكليز", "english"}},
	{"islamic", []string{"اسلام", "قران", "دينيه", "islamic", "quran"}},
	{"computer", []string{"حاسوب", "حاسب", "كمبيوتر", "معلوماتيه", "computer", "computers", "ict"}},
	{"science", []string{"علوم", "science", "sciences"}},
	{"social", []string{"اجتماع", "جغراف", "تاريخ", "وطنيه", "social", "history", "geography"}},
	{"art", []string{"فني", "رسم", "art", "arts", "drawing"}},
	{"music", []string{"موسيق", "نشيد", "اناشيد", "music"}},
}

var arabicFold = strings.NewReplacer(
	"أ", "ا", "إ", "ا", "آ", "ا", "ٱ", "ا",
	"ى", "ي", "ی", "ي", "ک", "ك", "ة", "ه",
)

// normalizeArabic trims s, removes Arabic diacritics and tatweel, folds أ إ آ ٱ to ا, ى to ي,
// ة to ه (and the Persian forms of ي and ك), lower-cases it and collapses runs of spaces.
func normalizeArabic(s string) string {
	s = strings.Map(func(r rune) rune {
		if (r >= 'ً' && r <= 'ْ') || r == 'ٰ' || r == 'ـ' {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
	return strings.Join(strings.Fields(arabicFold.Replace(s)), " ")
}

// SubjectKey maps a subject name to one of SubjectKeys, for the app to pick an icon.
func SubjectKey(subject string) string {
	name := normalizeArabic(subject)
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) }) {
		words[w] = true
	}
	for _, k := range subjectKeywords {
		for _, w := range k.words {
			if w[0] < 0x80 {
				if words[w] {
					return k.key
				}
			} else if strings.Contains(name, w) {
				return k.key
			}
		}
	}
	return "other"
}

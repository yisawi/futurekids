package handlers

import (
	"slices"
	"testing"
)

func TestSubjectKey(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"الرياضيات", "math"},
		{"اللغة العربية", "arabic"},
		{"العلوم", "science"},
		{"اللغة الإنكليزية", "english"},
		{"التربية الإسلامية", "islamic"},
		{"الاجتماعيات", "social"},
		{"التربية الفنية", "art"},
		{"التربية الرياضية", "pe"},
		{"الحاسوب", "computer"},

		{"رياضيات", "math"},
		{"الرياضة", "pe"},
		{"رياضه", "pe"},
		{"التربية البدنية", "pe"},
		{"الرِّيَاضِيَّات", "math"},
		{"الريـــاضيات", "math"},
		{"الحساب", "math"},

		{"اللغة الانكليزية", "english"},
		{"اللغة الإنجليزية", "english"},
		{"الانجليزي", "english"},
		{"  اللغة    العربيه  ", "arabic"},
		{"القراءة", "arabic"},
		{"العَرَبِيَّة", "arabic"},
		{"القرآن الكريم", "islamic"},
		{"القران", "islamic"},
		{"التربية الدينية", "islamic"},
		{"الاسلامية", "islamic"},
		{"التاريخ", "social"},
		{"الجغرافية", "social"},
		{"التربية الوطنية", "social"},
		{"المعلوماتية", "computer"},
		{"الحاسب الآلي", "computer"},
		{"كمبيوتر", "computer"},
		{"الرسم", "art"},
		{"الموسيقى", "music"},
		{"الأناشيد", "music"},
		{"النشيد", "music"},
		{"علوم", "science"},
		{"التربية الإسلاميه", "islamic"},
		{"اللغة العربيّة", "arabic"},
		{"الرياضيات ", "math"},
		{"ریاضیات", "math"},

		{"Mathematics", "math"},
		{"MATH", "math"},
		{"Maths", "math"},
		{"English Language", "english"},
		{"Englishness", "other"},
		{"Science", "science"},
		{"Computer Science", "computer"},
		{"Arabic", "arabic"},
		{"Islamic Studies", "islamic"},
		{"History", "social"},
		{"Art", "art"},
		{"Drawing", "art"},
		{"Music", "music"},
		{"Physical Education", "pe"},
		{"Smart", "other"},
		{"Mathematician-free club", "other"},

		{"نشاط حر", "other"},
		{"Club", "other"},
		{"Reading", "other"},
		{"", "other"},
		{"   ", "other"},
	} {
		if got := SubjectKey(tc.name); got != tc.want {
			t.Errorf("SubjectKey(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSubjectKeyMathIsNeverPE(t *testing.T) {
	for _, name := range []string{"الرياضيات", "رياضيات", "مادة الرياضيات", "الرياضيات والحساب"} {
		if got := SubjectKey(name); got != "math" {
			t.Errorf("%q = %q, want math", name, got)
		}
	}
	for _, name := range []string{"الرياضة", "التربية الرياضية", "رياضة بدنية", "التربيه الرياضيه"} {
		if got := SubjectKey(name); got != "pe" {
			t.Errorf("%q = %q, want pe", name, got)
		}
	}
}

func TestSubjectKeysAreTheTable(t *testing.T) {
	var keys []string
	for _, k := range subjectKeywords {
		keys = append(keys, k.key)
	}
	if want := append(keys, "other"); !slices.Equal(SubjectKeys, want) {
		t.Errorf("SubjectKeys = %v, want the keyword table order plus other: %v", SubjectKeys, want)
	}
}

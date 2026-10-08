package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"time"
	"unicode/utf8"
)

// baghdad is Asia/Baghdad: UTC+3 all year (Iraq has no daylight saving time).
var baghdad = time.FixedZone("Asia/Baghdad", 3*60*60)

var (
	boyNames  = []string{"أحمد", "محمد", "علي", "حسن", "حسين", "يوسف", "عمر", "مصطفى", "كرار", "طه", "حيدر", "مرتضى", "عباس", "جعفر", "سجاد"}
	girlNames = []string{"زينب", "فاطمة", "مريم", "سارة", "نور", "ليلى", "هدى", "رقية", "آية", "جنى", "زهراء", "دعاء", "رسل", "تبارك"}
	families  = []string{"تجريبي النجم", "تجريبي القمر", "تجريبي الشمس", "تجريبي البحر", "تجريبي النهر", "تجريبي الجبل", "تجريبي الوادي", "تجريبي السحاب", "تجريبي الربيع", "تجريبي الواحة"}
	subjects  = []string{"الرياضيات", "اللغة العربية", "العلوم", "اللغة الإنكليزية", "التربية الإسلامية", "الاجتماعيات", "التربية الفنية", "التربية الرياضية", "الحاسوب"}
	schoolDay = []string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس"}
	leaveNote = []string{"إجازة تجريبية - مرض", "إجازة تجريبية - ظرف عائلي", "إجازة تجريبية - موعد طبي"}
)

// class is a grade and section. emptyClass keeps students but never gets a schedule.
type class struct{ grade, section string }

func (c class) String() string { return c.grade + "/" + c.section }

var emptyClass = class{"G6", "B"}

// classOrder alternates grades so that consecutive students (siblings) are never in one grade.
var classOrder = func() []class {
	var out []class
	for _, section := range []string{"A", "B"} {
		for g := 1; g <= 6; g++ {
			out = append(out, class{fmt.Sprintf("G%d", g), section})
		}
	}
	return out
}()

type plannedParent struct {
	phone, name string
	children    []int
}

type plannedStudent struct {
	tag    int
	name   string
	class  class
	parent int
	rate   float64
}

type period struct {
	Day     string  `json:"day_of_week"`
	Number  int     `json:"period_number"`
	Subject string  `json:"subject_name"`
	Teacher *string `json:"teacher_name"`
}

type plannedLeave struct {
	student int
	date    time.Time
	notes   string
}

type plannedBanner struct {
	title       string
	actionLink  string
	active      bool
	filename    string
	contentType string
	data        []byte
}

type plan struct {
	parents   []plannedParent
	students  []plannedStudent
	schedules map[class][]period
	days      []time.Time
	leaves    []plannedLeave
	banners   []plannedBanner
}

func pick[T any](r *rand.Rand, list []T) T { return list[r.IntN(len(list))] }

// padTo appends filler words until s has at least min characters, never more than max.
func padTo(s string, min, max int, filler []string) string {
	for i := 0; utf8.RuneCountInString(s) < min; i++ {
		next := s + " " + filler[i%len(filler)]
		if utf8.RuneCountInString(next) > max {
			break
		}
		s = next
	}
	return s
}

func buildPlan(now time.Time) *plan {
	r := rand.New(rand.NewPCG(randomSeed, 1))
	p := &plan{schedules: map[class][]period{}}
	childCounts := make([]int, 37)
	for i := range childCounts {
		switch {
		case i < 28:
			childCounts[i] = 1
		case i < 36:
			childCounts[i] = 2
		default:
			childCounts[i] = 3
		}
	}
	for i, n := range childCounts {
		father, family := pick(r, boyNames), pick(r, families)
		parent := plannedParent{phone: fmt.Sprintf("+96470000020%02d", i+1), name: father + " " + family}
		if i == 0 {
			parent.name = padTo(parent.name, 245, 250, []string{"بن", "تجريبي", "الاسم", "الطويل", "جدا"})
		}
		for c := 0; c < n; c++ {
			idx := len(p.students)
			first := pick(r, boyNames)
			if r.IntN(2) == 0 {
				first = pick(r, girlNames)
			}
			name := first + " " + father + " " + family
			if i == 1 {
				name = padTo(first+" "+father+" عبد الرحمن "+family, 88, 92, []string{"تجريبي", "الاسم", "الطويل", "للاختبار"})
			}
			p.students = append(p.students, plannedStudent{
				tag: firstTag + idx, name: name, class: classOrder[idx%len(classOrder)], parent: i,
				rate: 0.6 + 0.4*r.Float64(),
			})
			parent.children = append(parent.children, idx)
		}
		p.parents = append(p.parents, parent)
	}

	for _, c := range classOrder {
		if c == emptyClass {
			continue
		}
		var periods []period
		for _, day := range schoolDay {
			n := 5 + r.IntN(2)
			for k := 1; k <= n; k++ {
				pd := period{Day: day, Number: k, Subject: pick(r, subjects)}
				if r.IntN(5) != 0 {
					teacher := fmt.Sprintf("معلم تجريبي %d", 1+r.IntN(20))
					if r.IntN(2) == 0 {
						teacher = fmt.Sprintf("معلمة تجريبية %d", 1+r.IntN(20))
					}
					pd.Teacher = &teacher
				}
				periods = append(periods, pd)
			}
		}
		p.schedules[c] = periods
	}

	today := now.In(baghdad)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	start := time.Date(today.Year(), today.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	for d := start; d.Before(today); d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Friday && d.Weekday() != time.Saturday {
			p.days = append(p.days, d)
		}
	}

	if len(p.days) > 0 {
		lr := rand.New(rand.NewPCG(randomSeed, 2))
		taken := map[string]bool{}
		for len(p.leaves) < 8 {
			s, d := lr.IntN(len(p.students)), p.days[lr.IntN(len(p.days))]
			key := fmt.Sprintf("%d %s", s, d.Format("2006-01-02"))
			if taken[key] {
				continue
			}
			taken[key] = true
			p.leaves = append(p.leaves, plannedLeave{student: s, date: d, notes: pick(lr, leaveNote)})
		}
	}

	p.banners = []plannedBanner{
		{title: "إعلان تجريبي - رحلة مدرسية", actionLink: "https://example.com/seed", active: true, filename: "seed-trip.png", contentType: "image/png", data: solidPNG(color.RGBA{0x1f, 0x7a, 0x8c, 0xff})},
		{title: "إعلان تجريبي - يوم مفتوح", active: false, filename: "seed-open-day.jpg", contentType: "image/jpeg", data: solidJPEG(color.RGBA{0xe0, 0x9f, 0x3e, 0xff})},
		{active: true, filename: "seed-untitled.jpg", contentType: "image/jpeg", data: solidJPEG(color.RGBA{0x54, 0x3d, 0x8c, 0xff})},
	}
	return p
}

func (p *plan) onLeave(student int, day time.Time) bool {
	for _, l := range p.leaves {
		if l.student == student && l.date.Equal(day) {
			return true
		}
	}
	return false
}

// attendance is one student's punches on one day. Times are Baghdad wall-clock times, as the
// device sends them.
type attendance struct {
	punches           []time.Time
	checkIn, checkOut bool
}

func (p *plan) attendance(student int, day time.Time) attendance {
	st := p.students[student]
	r := rand.New(rand.NewPCG(randomSeed, uint64(st.tag)<<32|uint64(day.Year()*10000+int(day.Month())*100+day.Day())))
	var a attendance
	if p.onLeave(student, day) || r.Float64() >= st.rate {
		return a
	}
	at := func(fromMin, toMin int) time.Time {
		m := fromMin + r.IntN(toMin-fromMin+1)
		return time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, r.IntN(60), 0, time.UTC)
	}
	duplicate := func(t time.Time) time.Time { return t.Add(time.Duration(1+r.IntN(4)) * time.Minute) }
	in := at(6*60+40, 9*60+20)
	a.punches, a.checkIn = append(a.punches, in), true
	if r.IntN(10) == 0 {
		a.punches = append(a.punches, duplicate(in))
	}
	if r.IntN(15) == 0 {
		a.punches = append(a.punches, at(9*60+35, 11*60+20))
	}
	if r.IntN(8) != 0 {
		out := at(11*60+35, 13*60+20)
		a.punches, a.checkOut = append(a.punches, out), true
		if r.IntN(12) == 0 {
			a.punches = append(a.punches, duplicate(out))
		}
	}
	if r.IntN(20) == 0 {
		a.punches = append(a.punches, at(13*60+40, 15*60+30))
	}
	return a
}

func solidImage(c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1200, 400))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	return img
}

func solidPNG(c color.Color) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, solidImage(c))
	return buf.Bytes()
}

func solidJPEG(c color.Color) []byte {
	var buf bytes.Buffer
	jpeg.Encode(&buf, solidImage(c), &jpeg.Options{Quality: 85})
	return buf.Bytes()
}

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const pinRequired = "parent_pin is required for a new parent"

type existingStudent struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	ParentPhone string  `json:"parent_phone"`
	RfidTag     string  `json:"rfid_tag"`
	Grade       *string `json:"grade"`
	Section     *string `json:"section"`
}

type counts struct {
	studentsCreated, studentsSkipped    int
	parentsNew, parentsExisting         int
	schedulesSet, schedulesUnchanged    int
	deviceCreated, deviceExisted        bool
	leavesSent                          int
	batchesSent, punchesSent            int
	expectedCheckIns, expectedCheckOuts int
	bannersCreated, bannersSkipped      int
	verifiedDay                         string
	verifiedStudents                    int
	replacedSchedules                   []string
	historyStudents                     int
}

type seeder struct {
	cfg      config
	out      io.Writer
	now      time.Time
	c        *client
	p        *plan
	step     string
	existing map[int]existingStudent
	ids      map[int]int
	targets  []int
	pinState map[int]string
	n        counts
	failed   int
	history  bool
}

func newSeeder(cfg config, out io.Writer, now time.Time) *seeder {
	return &seeder{cfg: cfg, out: out, now: now, c: newClient(cfg.baseURL), p: buildPlan(now),
		existing: map[int]existingStudent{}, ids: map[int]int{}, pinState: map[int]string{}}
}

func (s *seeder) printf(format string, args ...any) { fmt.Fprintf(s.out, format, args...) }

func (s *seeder) run() error {
	s.printf("seed-staging against %s\n", s.cfg.baseURL)
	if s.cfg.apply {
		s.printf("APPLY: sending writes.\n\n")
	} else {
		s.printf("DRY RUN: nothing will be changed. Run again with --apply to seed.\n\n")
	}
	s.step = "admin login"
	if err := s.c.login(s.cfg.username, s.cfg.password); err != nil {
		return err
	}
	s.step = "reading the existing students"
	var students struct {
		Data []existingStudent `json:"data"`
	}
	if err := s.c.call("GET", "/api/admin/students", nil, &students); err != nil {
		return err
	}
	for _, st := range students.Data {
		if tag, err := strconv.Atoi(st.RfidTag); err == nil {
			s.existing[tag] = st
		}
	}
	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"the fake device", s.seedDevice},
		{"students and parents", s.seedStudents},
		{"weekly schedules", s.seedSchedules},
		{"leaves", s.seedLeaves},
		{"attendance history", s.seedAttendance},
		{"banners", s.seedBanners},
		{"verification", s.verify},
	} {
		s.step = step.name
		if err := step.fn(); err != nil {
			return err
		}
	}
	s.printf("Settings: not touched (school_name and whatsapp_number are never changed).\n")
	s.summary()
	s.table()
	return nil
}

func (s *seeder) seedDevice() error {
	var devices struct {
		Data []struct {
			SerialNumber string `json:"serial_number"`
			IsActive     bool   `json:"is_active"`
		} `json:"data"`
	}
	if err := s.c.call("GET", "/api/admin/devices", nil, &devices); err != nil {
		return err
	}
	for _, d := range devices.Data {
		if d.SerialNumber != seedDevice {
			continue
		}
		s.n.deviceExisted = true
		if !d.IsActive {
			return fmt.Errorf("device %s exists but is disabled, so its punches would be dropped; enable it in the dashboard, then re-run", seedDevice)
		}
		s.printf("Device %s: already registered.\n", seedDevice)
		return nil
	}
	if !s.cfg.apply {
		s.printf("Device %s: will be registered (active, location \"Seed (fake)\").\n", seedDevice)
		return nil
	}
	err := s.c.call("POST", "/api/admin/devices", map[string]any{"serial_number": seedDevice, "location_name": "Seed (fake)", "is_active": true}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == http.StatusConflict {
		return fmt.Errorf("device %s was registered meanwhile; re-run", seedDevice)
	}
	if err != nil {
		return err
	}
	s.n.deviceCreated = true
	s.printf("Device %s: registered.\n", seedDevice)
	return nil
}

func (s *seeder) seedStudents() error {
	known := map[string]bool{}
	for _, st := range s.existing {
		known[st.ParentPhone] = true
	}
	for pi, parent := range s.p.parents {
		if known[parent.phone] {
			s.n.parentsExisting++
		} else {
			s.n.parentsNew++
		}
		for _, si := range parent.children {
			st := s.p.students[si]
			if ex, ok := s.existing[st.tag]; ok {
				s.n.studentsSkipped++
				s.ids[si] = ex.ID
				if s.cfg.resumeHistory {
					s.targets = append(s.targets, si)
				}
				continue
			}
			if !s.cfg.apply {
				s.n.studentsCreated++
				s.targets = append(s.targets, si)
				continue
			}
			id, err := s.createStudent(pi, st)
			var ae *apiError
			if errors.As(err, &ae) && ae.status == http.StatusConflict {
				s.n.studentsSkipped++
				continue
			}
			if err != nil {
				return fmt.Errorf("student rfid_tag %d: %w", st.tag, err)
			}
			s.n.studentsCreated++
			s.ids[si] = id
			s.targets = append(s.targets, si)
		}
	}
	verb := "will be created"
	if s.cfg.apply {
		verb = "created"
	}
	s.printf("Students (rfid_tag %d to %d): %d %s, %d already exist (skipped). rfid_tag 1001 to 1003 are never touched.\n", firstTag, lastTag, s.n.studentsCreated, verb, s.n.studentsSkipped)
	s.printf("Parents (+9647000002001 to +9647000002037): %d new, %d already registered (their PIN is never sent).\n", s.n.parentsNew, s.n.parentsExisting)
	return nil
}

func (s *seeder) createStudent(pi int, st plannedStudent) (int, error) {
	parent := s.p.parents[pi]
	body := map[string]any{"name": st.name, "parent_name": parent.name, "parent_phone": parent.phone,
		"rfid_tag": strconv.Itoa(st.tag), "grade": st.class.grade, "section": st.class.section}
	var resp struct {
		Data struct {
			ID int `json:"id"`
		} `json:"data"`
	}
	err := s.c.call("POST", "/api/admin/students", body, &resp)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == http.StatusBadRequest && strings.HasPrefix(ae.message, pinRequired) {
		body["parent_pin"] = seedPIN
		if err = s.c.call("POST", "/api/admin/students", body, &resp); err == nil {
			s.pinState[pi] = "set"
		}
	} else if err == nil && s.pinState[pi] == "" {
		s.pinState[pi] = "existing"
	}
	return resp.Data.ID, err
}

func (s *seeder) seedSchedules() error {
	var classes []class
	for c := range s.p.schedules {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].String() < classes[j].String() })
	for _, c := range classes {
		want := s.p.schedules[c]
		q := url.Values{"grade": {c.grade}, "section": {c.section}}
		var current struct {
			Data []period `json:"data"`
		}
		if err := s.c.call("GET", "/api/admin/schedule?"+q.Encode(), nil, &current); err != nil {
			return err
		}
		if samePeriods(current.Data, want) {
			s.n.schedulesUnchanged++
			continue
		}
		if len(current.Data) > 0 {
			s.n.replacedSchedules = append(s.n.replacedSchedules, fmt.Sprintf("%s (%d periods)", c, len(current.Data)))
		}
		if s.cfg.apply {
			if err := s.c.call("PUT", "/api/admin/schedule", map[string]any{"grade": c.grade, "section": c.section, "periods": want}, nil); err != nil {
				return fmt.Errorf("schedule %s: %w", c, err)
			}
		}
		s.n.schedulesSet++
	}
	verb := "will be set"
	if s.cfg.apply {
		verb = "set"
	}
	s.printf("Weekly schedules: %d classes %s, %d already identical; %s keeps no schedule on purpose (empty state).\n", s.n.schedulesSet, verb, s.n.schedulesUnchanged, emptyClass)
	for _, r := range s.n.replacedSchedules {
		s.printf("  REPLACES the existing schedule of %s.\n", r)
	}
	return nil
}

func samePeriods(a, b []period) bool {
	key := func(ps []period) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			teacher := "<none>"
			if p.Teacher != nil {
				teacher = *p.Teacher
			}
			out[i] = fmt.Sprintf("%s|%d|%s|%s", p.Day, p.Number, p.Subject, teacher)
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	if len(ka) != len(kb) {
		return false
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

func (s *seeder) isTarget(si int) bool {
	for _, t := range s.targets {
		if t == si {
			return true
		}
	}
	return false
}

func (s *seeder) seedLeaves() error {
	for _, l := range s.p.leaves {
		if !s.isTarget(l.student) {
			continue
		}
		if s.cfg.apply {
			body := map[string]any{"student_id": s.ids[l.student], "leave_date": l.date.Format("2006-01-02"), "notes": l.notes}
			if err := s.c.call("POST", "/api/admin/leaves", body, nil); err != nil {
				return fmt.Errorf("leave for rfid_tag %d on %s: %w", s.p.students[l.student].tag, l.date.Format("2006-01-02"), err)
			}
		}
		s.n.leavesSent++
	}
	verb := "will be recorded"
	if s.cfg.apply {
		verb = "recorded"
	}
	s.printf("Leaves: %d %s (only for students created in this run; no punch is sent on those days).\n", s.n.leavesSent, verb)
	return nil
}

func (s *seeder) seedAttendance() error {
	s.n.historyStudents = len(s.targets)
	if len(s.p.days) == 0 || len(s.targets) == 0 {
		s.printf("Attendance history: nothing to send.\n")
		return nil
	}
	for _, day := range s.p.days {
		var lines []string
		for _, si := range s.targets {
			tag := s.p.students[si].tag
			if tag < firstTag || tag > lastTag {
				return fmt.Errorf("refusing to send punches for rfid_tag %d", tag)
			}
			a := s.p.attendance(si, day)
			for _, t := range a.punches {
				lines = append(lines, fmt.Sprintf("%d\t%s\t1\t0", tag, t.Format("2006-01-02 15:04:05")))
			}
			if a.checkIn {
				s.n.expectedCheckIns++
			}
			if a.checkOut {
				s.n.expectedCheckOuts++
			}
		}
		if len(lines) == 0 {
			continue
		}
		if s.cfg.apply {
			if err := s.c.adms(seedDevice, strings.Join(lines, "\n")+"\n"); err != nil {
				return fmt.Errorf("attendance for %s: %w", day.Format("2006-01-02"), err)
			}
		}
		s.n.batchesSent++
		s.n.punchesSent += len(lines)
	}
	verb := "will be sent"
	if s.cfg.apply {
		verb = "sent"
	}
	first, last := s.p.days[0].Format("2006-01-02"), s.p.days[len(s.p.days)-1].Format("2006-01-02")
	s.printf("Attendance history: %d punches %s as device %s in %d batches (one per school day, %s to %s, never today) for %d students.\n",
		s.n.punchesSent, verb, seedDevice, s.n.batchesSent, first, last, len(s.targets))
	s.history = true
	s.printf("Expected parent notifications from it: %d check-in + %d check-out = %d (history rows dated now; pushes only reach phones the seeded parents have registered).\n",
		s.n.expectedCheckIns, s.n.expectedCheckOuts, s.n.expectedCheckIns+s.n.expectedCheckOuts)
	return nil
}

func (s *seeder) seedBanners() error {
	var banners struct {
		Data []struct {
			Title       *string `json:"title"`
			ContentType *string `json:"image_content_type"`
			Size        *int    `json:"image_size_bytes"`
		} `json:"data"`
	}
	if err := s.c.call("GET", "/api/admin/banners", nil, &banners); err != nil {
		return err
	}
	for _, b := range s.p.banners {
		exists := false
		for _, e := range banners.Data {
			switch {
			case b.title != "" && e.Title != nil && *e.Title == b.title:
				exists = true
			case b.title == "" && e.Title == nil && e.ContentType != nil && *e.ContentType == b.contentType && e.Size != nil && *e.Size == len(b.data):
				exists = true
			}
		}
		if exists {
			s.n.bannersSkipped++
			continue
		}
		if s.cfg.apply {
			parts := []formPart{{name: "image", filename: b.filename, contentType: b.contentType, data: b.data}, {name: "is_active", data: []byte(strconv.FormatBool(b.active))}}
			if b.title != "" {
				parts = append(parts, formPart{name: "title", data: []byte(b.title)})
			}
			if b.actionLink != "" {
				parts = append(parts, formPart{name: "action_link", data: []byte(b.actionLink)})
			}
			if err := s.c.multipart("POST", "/api/admin/banners", parts, nil); err != nil {
				return fmt.Errorf("banner %q: %w", b.title, err)
			}
		}
		s.n.bannersCreated++
	}
	verb := "will be uploaded"
	if s.cfg.apply {
		verb = "uploaded"
	}
	s.printf("Banners: %d %s, %d already exist (skipped).\n", s.n.bannersCreated, verb, s.n.bannersSkipped)
	return nil
}

// verify reads the admin daily report for the last seeded day and checks every student whose
// history was sent has the status the plan gives it.
func (s *seeder) verify() error {
	if !s.cfg.apply || s.n.batchesSent == 0 {
		return nil
	}
	day := s.p.days[len(s.p.days)-1]
	var report struct {
		Data []struct {
			StudentID int    `json:"student_id"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	if err := s.c.call("GET", "/api/admin/attendance?date="+day.Format("2006-01-02"), nil, &report); err != nil {
		return err
	}
	status := map[int]string{}
	for _, e := range report.Data {
		status[e.StudentID] = e.Status
	}
	for _, si := range s.targets {
		want := "Absent"
		if a := s.p.attendance(si, day); a.checkIn || a.checkOut {
			want = "Present"
		} else if s.p.onLeave(si, day) {
			want = "Excused"
		}
		if got := status[s.ids[si]]; got != want {
			return fmt.Errorf("rfid_tag %d on %s is %q in the daily report, want %q", s.p.students[si].tag, day.Format("2006-01-02"), got, want)
		}
	}
	s.n.verifiedDay, s.n.verifiedStudents = day.Format("2006-01-02"), len(s.targets)
	return nil
}

func (s *seeder) summary() {
	s.printf("\nSummary\n")
	s.printf("  students:  %d created, %d skipped\n", s.n.studentsCreated, s.n.studentsSkipped)
	s.printf("  parents:   %d new, %d existing\n", s.n.parentsNew, s.n.parentsExisting)
	s.printf("  schedules: %d set, %d unchanged\n", s.n.schedulesSet, s.n.schedulesUnchanged)
	device := "registered"
	switch {
	case s.n.deviceExisted:
		device = "already registered"
	case !s.n.deviceCreated && !s.cfg.apply:
		device = "to register"
	}
	s.printf("  device:    %s %s\n", seedDevice, device)
	s.printf("  leaves:    %d\n", s.n.leavesSent)
	s.printf("  punches:   %d in %d batches\n", s.n.punchesSent, s.n.batchesSent)
	s.printf("  banners:   %d created, %d skipped\n", s.n.bannersCreated, s.n.bannersSkipped)
	s.printf("  failed:    %d\n", s.failed)
	if s.n.verifiedDay != "" {
		s.printf("  verified:  the daily report of %s matches the plan for all %d students\n", s.n.verifiedDay, s.n.verifiedStudents)
	}
	if !s.cfg.apply {
		s.printf("\nDRY RUN: nothing was changed.\n")
	}
}

func (s *seeder) table() {
	s.printf("\nSeeded parents (fake data). Log in to the parent app with phone and PIN:\n")
	s.printf("phone | PIN | children (grade/section)\n")
	for pi, parent := range s.p.parents {
		pin := seedPIN
		if s.pinState[pi] == "existing" {
			pin = "unchanged (parent existed before this run)"
		}
		var kids []string
		for _, si := range parent.children {
			st := s.p.students[si]
			kids = append(kids, fmt.Sprintf("%s — %s", st.name, st.class))
		}
		s.printf("%s | %s | %s\n", parent.phone, pin, strings.Join(kids, " ; "))
	}
}

// stopped reports a run that failed midway: what was done, and how to continue safely.
func (s *seeder) stopped(err error) {
	s.failed = 1
	s.printf("\nSTOPPED during %s: %v\n", s.step, err)
	s.summary()
	s.printf("\nRe-running with --apply is safe: existing students, identical schedules, the device and existing banners are skipped, and no PIN is sent for an existing parent.\n")
	if s.cfg.apply && !s.history && len(s.targets) > 0 {
		var tags []string
		for _, si := range s.targets {
			tags = append(tags, strconv.Itoa(s.p.students[si].tag))
		}
		s.printf("The leaves and attendance history of rfid_tag %s were not completely sent. A plain re-run skips them; to send them (punches already stored are ignored by the server), run with --apply --resume-history.\n", strings.Join(tags, ", "))
	}
}

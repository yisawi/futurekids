// Package phone normalises Iraqi mobile numbers to one canonical form, +9647XXXXXXXXX (E.164).
package phone

import (
	"regexp"
	"strings"
)

// Ignored are removed before parsing: spaces (including no-break and thin spaces), dashes,
// parentheses, dots and the invisible direction marks that Arabic keyboards insert. The SQL
// copy in db/migrations/000028_normalize_phone_numbers.up.sql must remove the same set.
const Ignored = " \t-().   ‎‏‌‍"

var digits = strings.NewReplacer(
	"٠", "0", "١", "1", "٢", "2", "٣", "3", "٤", "4", "٥", "5", "٦", "6", "٧", "7", "٨", "8", "٩", "9",
	"۰", "0", "۱", "1", "۲", "2", "۳", "3", "۴", "4", "۵", "5", "۶", "6", "۷", "7", "۸", "8", "۹", "9",
)

var mobile = regexp.MustCompile(`^7[0-9]{9}$`)

// Normalize returns the canonical +9647XXXXXXXXX form of an Iraqi mobile number written as
// 07XXXXXXXXX, +9647XXXXXXXXX, 009647XXXXXXXXX or 9647XXXXXXXXX (a trunk 0 after the country
// code is tolerated), with ASCII, Arabic-Indic or Persian digits and any of the Ignored
// characters. ok is false for anything else.
func Normalize(raw string) (canonical string, ok bool) {
	s := strings.Map(func(r rune) rune {
		if strings.ContainsRune(Ignored, r) {
			return -1
		}
		return r
	}, digits.Replace(raw))
	switch {
	case strings.HasPrefix(s, "+964"):
		s = strings.TrimPrefix(s[4:], "0")
	case strings.HasPrefix(s, "00964"):
		s = strings.TrimPrefix(s[5:], "0")
	case strings.HasPrefix(s, "964"):
		s = strings.TrimPrefix(s[3:], "0")
	case strings.HasPrefix(s, "0"):
		s = s[1:]
	default:
		return "", false
	}
	if !mobile.MatchString(s) {
		return "", false
	}
	return "+964" + s, true
}

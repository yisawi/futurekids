// Command seed-staging fills the Staging environment with fake but realistic data for the
// Flutter developers, through the real admin API and the ADMS device endpoint only.
//
// Configuration comes from the environment: BASE_URL, ADMIN_USERNAME and ADMIN_PASSWORD.
// Without --apply it is a dry run that changes nothing. See README.md, "Seeding Staging".
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	stagingURL = "https://futurekids-staging.up.railway.app"
	seedPIN    = "314159"
	seedDevice = "SEED-FAKE-0001"
	firstTag   = 1004
	lastTag    = 1050
	randomSeed = 20261008
)

var localURL = regexp.MustCompile(`^http://localhost:[0-9]{1,5}$`)

type config struct {
	baseURL, username, password string
	apply, resumeHistory        bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr, time.Now()))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("seed-staging", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "send the writes (without it, a dry run that changes nothing)")
	allowLocal := fs.Bool("allow-local", false, "also accept BASE_URL http://localhost:<port> (tests only)")
	resume := fs.Bool("resume-history", false, "with --apply: also send the leaves and attendance history of seeded students that already exist (after a run that stopped midway)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "refused: unexpected arguments %q\n", fs.Args())
		return 2
	}
	cfg, err := loadConfig(getenv, *allowLocal)
	if err != nil {
		fmt.Fprintf(stderr, "refused: %v\n", err)
		return 2
	}
	cfg.apply, cfg.resumeHistory = *apply, *resume
	if cfg.resumeHistory && !cfg.apply {
		fmt.Fprintln(stderr, "refused: --resume-history needs --apply")
		return 2
	}
	s := newSeeder(cfg, stdout, now)
	if err := s.run(); err != nil {
		s.stopped(err)
		return 1
	}
	return 0
}

func loadConfig(getenv func(string) string, allowLocal bool) (config, error) {
	cfg := config{baseURL: getenv("BASE_URL"), username: getenv("ADMIN_USERNAME"), password: getenv("ADMIN_PASSWORD")}
	switch {
	case cfg.baseURL == stagingURL:
	case localURL.MatchString(cfg.baseURL) && allowLocal:
	case localURL.MatchString(cfg.baseURL):
		return cfg, errors.New("a local BASE_URL needs --allow-local")
	default:
		return cfg, fmt.Errorf("BASE_URL must be exactly %s (got %q)", stagingURL, cfg.baseURL)
	}
	var missing []string
	if cfg.username == "" {
		missing = append(missing, "ADMIN_USERNAME")
	}
	if cfg.password == "" {
		missing = append(missing, "ADMIN_PASSWORD")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("%s not set", strings.Join(missing, " and "))
	}
	return cfg, nil
}

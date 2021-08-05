package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/letsencrypt/boulder/cmd"
)

var raIssuanceLineRE = regexp.MustCompile(`Certificate request - successful JSON=(.*)`)

// TODO: Extract the "Valid for issuance: (true|false)" field too.
var vaCAALineRE = regexp.MustCompile(`Checked CAA records for ([a-z0-9-.*]+), \[Present: (true|false)`)

type issuanceEvent struct {
	SerialNumber string
	Names        []string
	Requester    int64

	issuanceTime time.Time
}

func openFile(path string) (*bufio.Scanner, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	reader = f
	if strings.HasSuffix(path, ".gz") {
		reader, err = gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
	}
	scanner := bufio.NewScanner(reader)
	return scanner, nil
}

func parseTimestamp(line string) (time.Time, error) {
	datestampText := line[0:32]
	datestamp, err := time.Parse(time.RFC3339, datestampText)
	if err != nil {
		return time.Time{}, err
	}
	return datestamp, nil
}

// loadIssuanceLog processes a single issuance (RA) log file. It returns a map
// of names to slices of timestamps at which certificates for those names were
// issued.
// TODO: plumb through earliest and latest for parity with old implementation.
func loadIssuanceLog(path string) (map[string][]time.Time, error) {
	scanner, err := openFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open %q: %w", path, err)
	}

	linesCount := 0
	issuancesCount := 0

	issuanceMap := map[string][]time.Time{}
	for scanner.Scan() {
		line := scanner.Text()
		linesCount++

		matches := raIssuanceLineRE.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		if len(matches) != 2 {
			return nil, fmt.Errorf("line %d: unexpected number of regex matches", linesCount)
		}

		var ie issuanceEvent
		err := json.Unmarshal([]byte(matches[1]), &ie)
		if err != nil {
			return nil, fmt.Errorf("line %d: failed to unmarshal JSON: %w", linesCount, err)
		}

		// Populate the issuance time from the syslog timestamp, rather than the
		// ResponseTime member of the JSON. This makes testing a lot simpler because
		// of how we mess with time sometimes. Given that these timestamps are
		// generated on the same system, they should be tightly coupled anyway.
		ie.issuanceTime, err = parseTimestamp(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: failed to parse timestamp: %w", linesCount, err)
		}

		issuancesCount++
		for _, name := range ie.Names {
			issuanceMap[name] = append(issuanceMap[name], ie.issuanceTime)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return issuanceMap, nil
}

// processCAALog processes a single CAA (VA) log file. It modifies the input map
// (of issuance names to times, as returned by `loadIssuanceLog`) to remove any
// timestamps which are covered by (i.e. less than 8 hours after) a CAA check
// for that name in the log file. It also prunes any names whose slice of
// issuance times becomes empty.
func processCAALog(path string, issuances map[string][]time.Time) error {
	scanner, err := openFile(path)
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", path, err)
	}

	linesCount := 0

	for scanner.Scan() {
		line := scanner.Text()
		linesCount++

		matches := vaCAALineRE.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		if len(matches) != 3 {
			return fmt.Errorf("line %d: unexpected number of regex matches", linesCount)
		}
		name := matches[1]
		present := matches[2]

		checkTime, err := parseTimestamp(line)
		if err != nil {
			return fmt.Errorf("line %d: failed to parse timestamp: %w", linesCount, err)
		}

		// TODO: Only remove covered issuance timestamps if the CAA check actually
		// said that we're allowed to issue (i.e. had "Valid for issuance: true").
		issuances[name] = removeCoveredTimestamps(issuances[name], checkTime)
		if len(issuances[name]) == 0 {
			delete(issuances, name)
		}

		// If the CAA check didn't find any CAA records for w.x.y.z, then that means
		// that we checked the CAA records for x.y.z, y.z, and z as well, and are
		// covered for any issuance for those names.
		if present == "false" {
			labels := strings.Split(name, ".")
			for i := 1; i < len(labels)-1; i++ {
				tailName := strings.Join(labels[i:], ".")
				issuances[tailName] = removeCoveredTimestamps(issuances[tailName], checkTime)
				if len(issuances[tailName]) == 0 {
					delete(issuances, tailName)
				}
			}
		}
	}

	return scanner.Err()
}

// removeCoveredTimestamps returns a new slice of timestamps which contains all
// timestamps that are *not* within 8 hours after the input timestamp.
// TODO: plumb through time-tolerance to account for slight slop.
func removeCoveredTimestamps(timestamps []time.Time, cover time.Time) []time.Time {
	r := make([]time.Time, len(timestamps))
	for _, ts := range timestamps {
		// Copy the timestamp into the results slice if it is before the covering
		// timestamp, or more than 8 hours after the covering timestamp (i.e. if
		// it is *not* covered by the covering timestamp).
		diff := ts.Sub(cover)
		if diff < 0 || diff > 8*time.Hour {
			ts := ts
			r = append(r, ts)
		}
	}
	return r
}

// formatErrors returns nil if the input map is empty. Otherwise, it returns an
// error containing a listing of every name and issuance time that was not
// covered by a CAA check.
func formatErrors(remaining map[string][]time.Time) error {
	if len(remaining) == 0 {
		return nil
	}

	messages := make([]string, len(remaining))
	for name, timestamps := range remaining {
		for _, timestamp := range timestamps {
			messages = append(messages, fmt.Sprintf("%v: %s", timestamp, name))
		}
	}

	sort.Strings(messages)
	return fmt.Errorf("\n%s", strings.Join(messages, "\n"))
}

func main() {
	logStdoutLevel := flag.Int("stdout-level", 6, "Minimum severity of messages to send to stdout")
	logSyslogLevel := flag.Int("syslog-level", 6, "Minimum severity of messages to send to syslog")
	raLog := flag.String("ra-log", "", "Path to a single boulder-ra log file")
	vaLogs := flag.String("va-logs", "", "List of paths to boulder-va logs, separated by commas")
	timeTolerance := flag.Duration("time-tolerance", 0, "How much slop to allow when comparing timestamps for ordering")
	earliestFlag := flag.String("earliest", "", "Day at which to start checking issuances "+
		"(inclusive). Formatted like '20060102' Optional. If specified, -latest is required.")
	latestFlag := flag.String("latest", "", "Day at which to stop checking issuances "+
		"(exclusive). Formatted like '20060102'. Optional. If specified, -earliest is required.")

	flag.Parse()

	if *timeTolerance < 0 {
		cmd.Fail("value of -time-tolerance must be non-negative")
	}

	var earliest time.Time
	var latest time.Time
	if *earliestFlag != "" || *latestFlag != "" {
		if *earliestFlag == "" || *latestFlag == "" {
			cmd.Fail("-earliest and -latest must be both set or both unset")
		}
		var err error
		earliest, err = time.Parse("20060102", *earliestFlag)
		cmd.FailOnError(err, "value of -earliest could not be parsed as date")
		latest, err = time.Parse("20060102", *latestFlag)
		cmd.FailOnError(err, "value of -latest could not be parsed as date")

		if earliest.After(latest) {
			cmd.Fail("earliest date must be before latest date")
		}
	}

	_ = cmd.NewLogger(cmd.SyslogConfig{
		StdoutLevel: *logStdoutLevel,
		SyslogLevel: *logSyslogLevel,
	})

	// Build a map from hostnames to times at which those names were issued for.
	issuanceMap, err := loadIssuanceLog(*raLog)
	cmd.FailOnError(err, "failed to load issuance logs")

	// Try to pare the issuance map down to nothing by removing every entry which
	// is covered by a CAA check.
	for _, vaLog := range strings.Split(*vaLogs, ",") {
		err = processCAALog(vaLog, issuanceMap)
		cmd.FailOnError(err, "failed to process CAA checking logs")
	}

	err = formatErrors(issuanceMap)
	cmd.FailOnError(err, "the following issuances were missing CAA checks")
}

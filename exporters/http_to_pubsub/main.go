// This program copies rows from a Hydrolix table to a Google Pub/Sub topic. It
// queries the Hydrolix HTTP query API one time window at a time and publishes
// each row as one message. A full walkthrough is at
// https://docs.hydrolix.io/latest/exploring-data/export-to-google-pubsub/.
//
// This approach suits low-throughput tables and summary tables. Higher volumes
// could need parallel queries and a saved checkpoint.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

const (
	// SQL literal for a window boundary. Windows are whole seconds, and this
	// literal works for datetime columns at any resolution.
	sqlTimeFormat = "2006-01-02 15:04:05"
	// The query run when --query isn't set.
	defaultQuery = "SELECT * FROM {table} WHERE {column} >= {start} AND {column} < {end} ORDER BY {column}"
)

var (
	hdxQueryURL  string
	hdxToken     string
	hdxUser      string
	hdxPassword  string
	hdxTable     string
	timeColumn   string
	queryFlag    string
	gcpProjectID string
	gcpTopicID   string
	window       time.Duration
	delay        time.Duration
	queryTimeout time.Duration
	startFlag    string
	endFlag      string

	queryTemplate string
	startTime     time.Time
	endTime       time.Time
)

func init() {
	flag.StringVar(&hdxQueryURL, "hdx_url", "", "Hydrolix query URL, such as https://hostname.hydrolix.live/query")
	flag.StringVar(&hdxToken, "hdx_token", "", "Hydrolix bearer token (default: the HDX_TOKEN environment variable)")
	flag.StringVar(&hdxUser, "hdx_user", "", "Hydrolix username, if not using a token")
	flag.StringVar(&hdxPassword, "hdx_password", "", "Hydrolix password, if not using a token (default: the HDX_PASSWORD environment variable)")
	flag.StringVar(&hdxTable, "hdx_table", "", "Table to export, in PROJECT.TABLE notation")
	flag.StringVar(&timeColumn, "time_column", "timestamp", "The table's primary datetime column")
	flag.StringVar(&queryFlag, "query", "", "SQL to run for each window instead of the default SELECT *. Use {start} and {end} for the window boundaries, and leave out FORMAT")
	flag.StringVar(&gcpProjectID, "gcp_project", "", "Google Cloud project ID")
	flag.StringVar(&gcpTopicID, "gcp_topic", "", "Pub/Sub topic ID. The topic must exist")
	flag.DurationVar(&window, "window", time.Minute, "Length of each time window, in whole seconds")
	flag.DurationVar(&delay, "delay", time.Minute, "How long to wait after a window ends before querying it, for late rows to arrive")
	flag.DurationVar(&queryTimeout, "query_timeout", 5*time.Minute, "How long to wait for one query")
	flag.StringVar(&startFlag, "start", "", "Start of the first window, in RFC 3339 format such as 2026-09-01T00:00:00Z (default: now minus --delay, rounded down to a whole --window)")
	flag.StringVar(&endFlag, "end", "", "Stop at this time, in RFC 3339 format. Rows at exactly this time aren't exported (default: run until stopped)")
}

func parseArgs() error {
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q. Check for a missing backslash or a flag without --", flag.Arg(0))
	}

	for name, value := range map[string]string{
		"hdx_url": hdxQueryURL, "gcp_project": gcpProjectID, "gcp_topic": gcpTopicID,
	} {
		if value == "" {
			return fmt.Errorf("--%s must be set", name)
		}
	}
	if u, err := url.ParseRequestURI(hdxQueryURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("--hdx_url must be a full URL, such as https://hostname.hydrolix.live/query")
	}

	if hdxToken == "" {
		hdxToken = os.Getenv("HDX_TOKEN")
	}
	if hdxPassword == "" {
		hdxPassword = os.Getenv("HDX_PASSWORD")
	}
	if hdxToken == "" && (hdxUser == "" || hdxPassword == "") {
		return errors.New("set a token (HDX_TOKEN or --hdx_token), or both --hdx_user and HDX_PASSWORD")
	}

	if queryFlag != "" {
		queryTemplate = queryFlag
	} else if hdxTable == "" {
		return errors.New("set --hdx_table, or write your own --query")
	} else {
		queryTemplate = strings.NewReplacer("{table}", hdxTable, "{column}", timeColumn).Replace(defaultQuery)
	}
	if !strings.Contains(queryTemplate, "{start}") || !strings.Contains(queryTemplate, "{end}") {
		return errors.New("--query must use both {start} and {end}")
	}

	if window < time.Second || window%time.Second != 0 {
		return errors.New("--window must be a whole number of seconds, at least 1s")
	}
	if delay < 0 || queryTimeout <= 0 {
		return errors.New("--delay can't be negative, and --query_timeout must be positive")
	}

	var err error
	if startFlag == "" {
		// Line windows up with the clock. A one-minute window covers one whole
		// minute.
		startTime = time.Now().Add(-delay).Truncate(window)
	} else if startTime, err = time.Parse(time.RFC3339, startFlag); err != nil {
		return fmt.Errorf("can't read --start: %v", err)
	} else if startTime.Nanosecond() != 0 {
		return errors.New("--start must be a whole second")
	}
	if endFlag != "" {
		if endTime, err = time.Parse(time.RFC3339, endFlag); err != nil {
			return fmt.Errorf("can't read --end: %v", err)
		}
		if !endTime.After(startTime) {
			return fmt.Errorf("--end must be later than the start, %s", startTime.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// buildQuery returns the SQL for one window. A window includes its start and
// excludes its end. A row is never in two windows.
func buildQuery(start, end time.Time) string {
	quote := func(t time.Time) string { return "'" + t.UTC().Format(sqlTimeFormat) + "'" }
	return strings.NewReplacer("{start}", quote(start), "{end}", quote(end)).Replace(queryTemplate) +
		" FORMAT JSONEachRow"
}

// exportWindow queries one window and publishes each row as a message. It
// returns the number of rows published.
func exportWindow(ctx context.Context, httpClient *http.Client, publisher *pubsub.Publisher, start, end time.Time) (int, error) {
	query := buildQuery(start, end)
	log.Println(query)

	// Finish the window even if the program is stopping. A stop never leaves
	// a window half published.
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(qctx, http.MethodPost, hdxQueryURL, strings.NewReader(query))
	if err != nil {
		return 0, err
	}
	if hdxToken != "" {
		req.Header.Set("Authorization", "Bearer "+hdxToken)
	} else {
		req.SetBasicAuth(hdxUser, hdxPassword)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := bufio.NewReader(resp.Body).Peek(2000)
		return 0, fmt.Errorf("Hydrolix returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	// Publish each row as it's read. Every message carries its window, which
	// tells a consumer where it came from.
	attrs := map[string]string{
		"window_start": start.UTC().Format(time.RFC3339),
		"window_end":   end.UTC().Format(time.RFC3339),
	}
	var results []*pubsub.PublishResult
	rows := bufio.NewScanner(resp.Body)
	rows.Buffer(make([]byte, 64*1024), pubsub.MaxPublishRequestBytes)
	for rows.Scan() {
		data := append([]byte(nil), rows.Bytes()...)
		results = append(results, publisher.Publish(context.WithoutCancel(ctx), &pubsub.Message{Data: data, Attributes: attrs}))
	}
	scanErr := rows.Err()

	// Wait for every publish, even if a row failed.
	for _, r := range results {
		if _, err := r.Get(context.WithoutCancel(ctx)); err != nil {
			if errors.Is(err, pubsub.ErrOversizedMessage) {
				return 0, errors.New("a row is larger than Pub/Sub accepts. Select fewer columns")
			}
			return 0, fmt.Errorf("publish failed: %v", err)
		}
	}
	if errors.Is(scanErr, bufio.ErrTooLong) {
		return 0, errors.New("a row is larger than Pub/Sub accepts. Select fewer columns")
	}
	if scanErr != nil {
		return 0, fmt.Errorf("reading the query result: %v", scanErr)
	}
	return len(results), nil
}

// waitUntil sleeps until t, and returns false if the program is stopped first.
func waitUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func run(ctx context.Context) error {
	client, err := pubsub.NewClient(ctx, gcpProjectID)
	if err != nil {
		return fmt.Errorf("can't create Pub/Sub client: %v", err)
	}
	defer client.Close()
	publisher := client.Publisher(gcpTopicID)
	defer publisher.Stop()
	httpClient := &http.Client{}

	for start := startTime; endTime.IsZero() || start.Before(endTime); {
		end := start.Add(window)
		if !endTime.IsZero() && end.After(endTime) {
			end = endTime
		}

		// Wait until the window has ended and late rows have had time to
		// arrive. A window in the past runs right away. The exporter catches
		// up after a backfill or a slow query.
		if time.Until(end.Add(delay)) > window {
			log.Printf("Waiting until %s to query the next window.", end.Add(delay).Format(time.RFC3339))
		}
		if !waitUntil(ctx, end.Add(delay)) {
			log.Printf("Stopped. To resume, run again with --start=%s", start.UTC().Format(time.RFC3339))
			return nil
		}

		n, err := exportWindow(ctx, httpClient, publisher, start, end)
		if err != nil {
			return fmt.Errorf("%v\nTo resume, run again with --start=%s", err, start.UTC().Format(time.RFC3339))
		}
		log.Printf("Published %d rows.", n)
		start = end
	}
	log.Println("Reached --end. Done.")
	return nil
}

func main() {
	if err := parseArgs(); err != nil {
		log.Fatal(err)
	}

	// Stop cleanly on Ctrl+C or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		log.Fatal(err)
	}
}

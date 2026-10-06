// This program copies rows from a Hydrolix table to a Google Pub/Sub topic. It
// queries the Hydrolix HTTP query API one time window at a time and publishes
// each row as one message. A full walkthrough is at
// https://docs.hydrolix.io/latest/exploring-data/export-to-google-pubsub/.
//
// This approach suits low-throughput tables and summary tables. Higher volumes
// could need parallel queries.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

const (
	// SQL literal for a window boundary, in UTC. The query head runs in UTC.
	sqlTimeFormat = "2006-01-02 15:04:05"
	// The query run when --query isn't set.
	defaultQuery = "SELECT * FROM {table} WHERE {column} >= {start} AND {column} < {end} ORDER BY {column}"
	// Largest row the exporter publishes. Pub/Sub's limit is 10,000,000
	// bytes for the whole request, and the attributes and topic name take
	// some of that.
	maxRowBytes = pubsub.MaxPublishRequestBytes - 1024
	// Retries for a query that fails with a temporary error.
	queryRetries = 3
	// Publish results to wait for at a time.
	publishBatch = 1000
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

	// True while a window is being queried or published.
	busy atomic.Bool
	// Start of the window in progress, in Unix seconds, for the resume value
	// logged when a second signal stops the exporter at once.
	currentStart atomic.Int64

	errStopped     = errors.New("stopped")
	errRowTooLarge = errors.New("a row is larger than Pub/Sub accepts. Select fewer columns")
)

func init() {
	flag.StringVar(&hdxQueryURL, "hdx_url", "", "Hydrolix query URL, such as https://hostname.hydrolix.live/query")
	flag.StringVar(&hdxToken, "hdx_token", "", "Hydrolix bearer token (default: the HDX_TOKEN environment variable)")
	flag.StringVar(&hdxUser, "hdx_user", "", "Hydrolix username, if not using a token")
	flag.StringVar(&hdxPassword, "hdx_password", "", "Hydrolix password, if not using a token (default: the HDX_PASSWORD environment variable)")
	flag.StringVar(&hdxTable, "hdx_table", "", "Table to export, in PROJECT.TABLE notation. Used by the default query only")
	flag.StringVar(&timeColumn, "time_column", "timestamp", "The table's primary datetime column. Used by the default query only")
	flag.StringVar(&queryFlag, "query", "", "SQL to run for each window instead of the default SELECT *. Filter with >= {start} AND < {end}, without quotes, and leave out FORMAT")
	flag.StringVar(&gcpProjectID, "gcp_project", "", "Google Cloud project ID")
	flag.StringVar(&gcpTopicID, "gcp_topic", "", "Pub/Sub topic ID. The topic must exist")
	flag.DurationVar(&window, "window", time.Minute, "Length of each time window, in whole seconds")
	flag.DurationVar(&delay, "delay", time.Minute, "How long to wait after a window ends before querying it, for late rows to arrive")
	flag.DurationVar(&queryTimeout, "query_timeout", 5*time.Minute, "Time limit for one query, including reading its result")
	flag.StringVar(&startFlag, "start", "", "Start of the first window, in RFC 3339 format such as 2026-09-01T00:00:00Z (default: now minus --delay, rounded down to a whole --window)")
	flag.StringVar(&endFlag, "end", "", "Stop at this time, in RFC 3339 format. With the default query, rows at exactly this time aren't exported (default: run until stopped)")
}

func parseArgs() error {
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q. Check for a missing backslash or a flag without --", flag.Arg(0))
	}

	for _, f := range []struct{ name, value string }{
		{"hdx_url", hdxQueryURL}, {"gcp_project", gcpProjectID}, {"gcp_topic", gcpTopicID},
	} {
		if f.value == "" {
			return fmt.Errorf("--%s must be set", f.name)
		}
	}
	if u, err := url.ParseRequestURI(hdxQueryURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
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
		queryTemplate = strings.TrimRight(strings.TrimSpace(queryFlag), ";")
	} else if hdxTable == "" {
		return errors.New("set --hdx_table, or set --query")
	} else if strings.ContainsAny(hdxTable+timeColumn, "{}") {
		return errors.New("--hdx_table and --time_column can't contain { or }")
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
		if window > 24*time.Hour {
			return errors.New("set --start when --window is longer than 24h")
		}
		// Line windows up with the clock, counted from the Unix epoch. A
		// one-minute window covers one whole minute.
		epoch := time.Unix(0, 0).UTC()
		startTime = epoch.Add(time.Now().Add(-delay).Sub(epoch) / window * window)
	} else if startTime, err = time.Parse(time.RFC3339, startFlag); err != nil {
		return fmt.Errorf("can't read --start: %v", err)
	} else if startTime.Nanosecond() != 0 {
		return errors.New("--start must be a whole second")
	}
	if endFlag != "" {
		if endTime, err = time.Parse(time.RFC3339, endFlag); err != nil {
			return fmt.Errorf("can't read --end: %v", err)
		}
		if endTime.Nanosecond() != 0 {
			return errors.New("--end must be a whole second")
		}
		if !endTime.After(startTime) {
			return fmt.Errorf("--end must be later than the start, %s", utc(startTime))
		}
	}

	// Each window is held in a temporary file. Check the folder now, before
	// any query runs.
	f, err := createTemp()
	if err != nil {
		return fmt.Errorf("can't write a temporary file: %v. Set TMPDIR to a writable folder", err)
	}
	f.Close()
	return nil
}

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// createTemp creates a temporary file and removes its name at once. The open
// file still works, and nothing is left behind however the program stops.
func createTemp() (*os.File, error) {
	f, err := os.CreateTemp("", "hdx-pubsub-export-*.ndjson")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name())
	return f, nil
}

// buildQuery returns the SQL for one window. FORMAT goes on its own line. A
// comment at the end of the query can't hide it.
func buildQuery(start, end time.Time) string {
	quote := func(t time.Time) string { return "'" + t.UTC().Format(sqlTimeFormat) + "'" }
	return strings.NewReplacer("{start}", quote(start), "{end}", quote(end)).Replace(queryTemplate) +
		"\nFORMAT JSONEachRow"
}

// queryException returns the error from a row that holds only an
// "exception" field.
func queryException(row []byte) (string, bool) {
	if !bytes.HasPrefix(row, []byte(`{"exception"`)) {
		return "", false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(row, &fields) != nil || len(fields) != 1 {
		return "", false
	}
	var msg string
	if json.Unmarshal(fields["exception"], &msg) != nil {
		return string(fields["exception"]), true
	}
	return msg, true
}

// errRetry marks a query failure worth retrying: a server error, a busy
// server, or a broken connection.
type errRetry struct{ err error }

func (e errRetry) Error() string { return e.err.Error() }

// readLine reads one line, and stops early if it grows past limit bytes.
func readLine(in *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := in.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return nil, errRowTooLarge
		}
		line = append(line, chunk...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// fetchWindow runs the query for one window and writes each row to a temporary
// file, checking every row first. Nothing is published until the whole window
// has been read. A window that fails here publishes nothing, and running it
// again doesn't duplicate rows.
func fetchWindow(httpClient *http.Client, query string) (*os.File, int, error) {
	f, err := createTemp()
	if err != nil {
		return nil, 0, err
	}
	fail := func(err error) (*os.File, int, error) {
		f.Close()
		return nil, 0, err
	}

	// The query isn't cancelled by a stop request. The exporter finishes the
	// window it's working on.
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	timedOut := func(err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("the query took longer than --query_timeout (%s)", queryTimeout)
		}
		return errRetry{err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hdxQueryURL, strings.NewReader(query))
	if err != nil {
		return fail(err)
	}
	if hdxToken != "" {
		req.Header.Set("Authorization", "Bearer "+hdxToken)
	} else {
		req.SetBasicAuth(hdxUser, hdxPassword)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fail(timedOut(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		err := fmt.Errorf("Hydrolix returned %s", resp.Status)
		if msg = bytes.TrimSpace(msg); len(msg) > 0 {
			err = fmt.Errorf("%v: %s", err, msg)
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
			return fail(errRetry{err})
		}
		return fail(err)
	}
	// A response with no length that isn't sent in chunks ends when the
	// connection closes, and a cut-off result can't be detected.
	if resp.ProtoMajor == 1 && resp.ContentLength < 0 && len(resp.TransferEncoding) == 0 {
		return fail(errors.New("the query result has no length and isn't chunked. A cut-off result can't be detected"))
	}

	out := bufio.NewWriter(f)
	in := bufio.NewReader(resp.Body)
	rows := 0
	for {
		line, err := readLine(in, maxRowBytes+2)
		if errors.Is(err, errRowTooLarge) {
			return fail(err)
		}
		if err != nil && err != io.EOF {
			return fail(timedOut(fmt.Errorf("reading the query result: %v", err)))
		}
		row := bytes.TrimSpace(line)
		if err == io.EOF && len(row) > 0 {
			return fail(errRetry{errors.New("the query result ended partway through a row")})
		}
		if len(row) > 0 {
			if len(row) > maxRowBytes {
				return fail(errRowTooLarge)
			}
			if row[0] != '{' || !json.Valid(row) {
				return fail(fmt.Errorf("the query result has a line that isn't a JSON row: %.200s", row))
			}
			// A query that fails after it starts sending rows can end with
			// an object holding only the error.
			if msg, ok := queryException(row); ok {
				return fail(fmt.Errorf("the query failed partway: %.500s", msg))
			}
			out.Write(row)
			out.WriteByte('\n')
			rows++
		}
		if err == io.EOF {
			break
		}
	}
	if err := out.Flush(); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return f, rows, nil
}

// publishRows publishes each row in the file as one message. Every message
// carries its window, which tells a consumer where it came from.
func publishRows(publisher *pubsub.Publisher, f *os.File, start, end time.Time) error {
	ctx := context.Background()
	attrs := map[string]string{"window_start": utc(start), "window_end": utc(end)}
	var results []*pubsub.PublishResult
	wait := func() error {
		for _, r := range results {
			if _, err := r.Get(ctx); err != nil {
				return fmt.Errorf("publish failed: %v", err)
			}
		}
		results = results[:0]
		return nil
	}

	in := bufio.NewReader(f)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			data := bytes.TrimSuffix(line, []byte("\n"))
			results = append(results, publisher.Publish(ctx, &pubsub.Message{Data: data, Attributes: attrs}))
			if len(results) == publishBatch {
				if err := wait(); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return wait()
}

// exportWindow queries one window, retrying temporary errors, then publishes
// its rows. It returns the number of rows published, or errStopped if the
// program was stopped while waiting to retry.
func exportWindow(ctx context.Context, httpClient *http.Client, publisher *pubsub.Publisher, start, end time.Time) (int, error) {
	query := buildQuery(start, end)
	log.Println(strings.ReplaceAll(query, "\n", " "))

	var f *os.File
	var rows int
	var err error
	for attempt := 0; ; attempt++ {
		f, rows, err = fetchWindow(httpClient, query)
		var retry errRetry
		if err == nil || !errors.As(err, &retry) || attempt == queryRetries {
			break
		}
		wait := time.Duration(2<<attempt) * time.Second
		log.Printf("%v. Retrying in %s.", err, wait)
		busy.Store(false)
		ok := waitUntil(ctx, time.Now().Add(wait))
		busy.Store(true)
		if !ok {
			return 0, errStopped
		}
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if err := publishRows(publisher, f, start, end); err != nil {
		return 0, err
	}
	return rows, nil
}

// waitUntil sleeps until t, and returns false if the program is stopped first.
func waitUntil(ctx context.Context, t time.Time) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		d := time.Until(t)
		if d <= 0 {
			return true
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func run(ctx context.Context) error {
	client, err := pubsub.NewClient(context.Background(), gcpProjectID)
	if err != nil {
		return fmt.Errorf("can't create Pub/Sub client: %v", err)
	}
	defer client.Close()
	publisher := client.Publisher(gcpTopicID)
	// Limit unsent messages to 1,000 or 100 MB. Total memory use can be
	// higher.
	publisher.PublishSettings.FlowControlSettings = pubsub.FlowControlSettings{
		MaxOutstandingMessages: 1000,
		MaxOutstandingBytes:    100 << 20,
		LimitExceededBehavior:  pubsub.FlowControlBlock,
	}
	defer publisher.Stop()
	httpClient := &http.Client{
		// Don't follow redirects. A redirect would turn the POST into a GET
		// without the query.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	waited := false
	for start := startTime; endTime.IsZero() || start.Before(endTime); {
		end := start.Add(window)
		if !endTime.IsZero() && end.After(endTime) {
			end = endTime
		}
		currentStart.Store(start.Unix())

		// Wait until the window has ended and late rows have had time to
		// arrive. A window in the past runs right away. The exporter catches
		// up after a backfill or a slow query.
		ready := end.Add(delay)
		if time.Until(ready) > 0 && (!waited || time.Until(ready) > window) {
			log.Printf("Waiting until %s to query the window starting %s.", utc(ready), utc(start))
			waited = true
		}
		if !waitUntil(ctx, ready) {
			log.Printf("Stopped. To resume, run again with --start=%s", utc(start))
			return nil
		}

		busy.Store(true)
		n, err := exportWindow(ctx, httpClient, publisher, start, end)
		busy.Store(false)
		if errors.Is(err, errStopped) {
			log.Printf("Stopped. To resume, run again with --start=%s", utc(start))
			return nil
		}
		if err != nil {
			return fmt.Errorf("%v\nTo resume, run again with --start=%s", err, utc(start))
		}
		noun := "rows"
		if n == 1 {
			noun = "row"
		}
		log.Printf("Published %d %s. Next --start=%s", n, noun, utc(end))
		start = end
	}
	log.Println("Reached --end. Done.")
	return nil
}

func main() {
	if err := parseArgs(); err != nil {
		log.Fatal(err)
	}

	// The first Ctrl+C or SIGTERM stops after the window in progress. A
	// second one stops at once.
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	finished := make(chan struct{})
	go func() {
		select {
		case <-signals:
		case <-finished:
			return
		}
		if busy.Load() {
			log.Println("Stopping after the window in progress. Press Ctrl+C again to stop at once.")
		}
		cancel()
		select {
		case sig := <-signals:
			log.Printf("Stopped at once. To resume, run again with --start=%s", utc(time.Unix(currentStart.Load(), 0)))
			if sig == syscall.SIGTERM {
				os.Exit(143)
			}
			os.Exit(130)
		case <-finished:
		}
	}()
	err := run(ctx)
	close(finished)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
}

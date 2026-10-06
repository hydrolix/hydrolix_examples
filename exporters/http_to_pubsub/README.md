# Hydrolix to Google Pub/Sub exporter

This program copies rows from a Hydrolix table to a Google Pub/Sub topic. It queries the Hydrolix HTTP query API one time window at a time and publishes each row as one message.

For step-by-step instructions, see [Export Data to Google Pub/Sub](https://docs.hydrolix.io/latest/exploring-data/export-to-google-pubsub/) in the Hydrolix documentation.

## Build

```bash
go build -o hdx-pubsub-export .
./hdx-pubsub-export -h
```

## How it works

- The exporter splits time into windows (`--window`, one minute by default). With the default query, a window includes rows at its start time and leaves out rows at its end time.
- It waits `--delay` (one minute by default) after a window ends before it queries it. New rows take a short time to become ready to query.
- It reads the whole query result for a window and checks every row before publishing any of them. Rows are held in a temporary file while it checks them. If the query or a row fails, nothing from that window is published.
- It publishes each row as one message. Each message has two attributes, `window_start` and `window_end`, in RFC 3339 format.
- With `--start`, it begins at that time. Windows in the past run right away, then it follows new data. With `--end`, it stops there.
- `--query` replaces the default `SELECT *` with custom SQL. Filter on the time column with `>= {start} AND < {end}`, without quotes around them, and leave out `FORMAT`.
- After each window, it logs the `--start` value for the next one. A stop or an error logs the `--start` value to resume from.
- The first Ctrl+C or SIGTERM stops it after the window in progress. A second one stops it at once and logs the `--start` value to resume from.

## Run it in production

The commands on the docs page are for trying the exporter. Before relying on it in production, check these.

- **Use a service account token**. A login token expires after 24 hours. A [service account token](https://docs.hydrolix.io/latest/self-managed/advanced-configuration/authentication-and-authorization/account-types/service-accounts-howto/#service-account-tokens) lasts 365 days by default, but it also expires. The exporter stops when its token stops working. Give the service account a role that can query only the tables it exports. Pass the token in the `HDX_TOKEN` environment variable, not as `--hdx_token`, which shows in the process list.
- **Give the Google Cloud identity the Pub/Sub Publisher role**. `roles/pubsub.publisher` can publish to a topic. The exporter doesn't create topics. Create the topic ahead of time.
- **Plan for restarts**. The exporter doesn't save its place. Run it under something that restarts it with the last `--start` value it logged. Without `--start`, it starts one `--delay` before the current time, rounded down to a whole `--window`, and rows from the gap aren't exported.
- **Allow time to stop**. A window in progress finishes before the exporter stops, which can take up to `--query_timeout` plus the time to publish. If the exporter runs in Kubernetes, set the pod's termination grace period to cover it.
- **Expect some retries and duplicates**. The exporter retries a query three times after a temporary error, such as a busy server (HTTP 429) or a broken connection, then stops. A query that runs longer than `--query_timeout` isn't retried. A failure while publishing can leave part of a window published, and resuming publishes that window again. Pub/Sub can also deliver a message more than once. The `window_start` and `window_end` attributes tell a consumer which window a message came from. To remove duplicates, include a column that identifies each row.
- **Don't rely on order**. Messages don't carry an ordering key, and the exporter publishes rows in batches. Pub/Sub doesn't guarantee their order.
- **Set `--delay` for the table**. Rows that arrive after their window is queried aren't exported. With default stream settings, a partition can stay open for up to 20 seconds (`hot_data_max_open_seconds`), and it takes more time to upload and add to the catalog. The one-minute default leaves room for that. If rows reach Hydrolix late, raise `--delay` to match.
- **Keep queries fast**. Each window is one query. `--query_timeout` (five minutes by default) limits how long it can take, including reading its result. Select only the needed columns.
- **Mind the message size limit**. Pub/Sub limits a publish request to 10,000,000 bytes, including the message's attributes. The exporter stops on a row close to that size, before publishing anything from that window. Resuming reaches the same row again. Change `--query` to select fewer columns.
- **Give it disk space**. Each window's rows are held in a temporary file in `TMPDIR` until they're published. The folder needs room for the largest window. In Kubernetes, a read-only root file system needs a writable volume, such as an `emptyDir`, with `TMPDIR` pointing to it.
- **Export summary tables with a query**. `SELECT *` returns a summary table's stored partial results, not its values. Use `--query` with `GROUP BY` and `HAVING`, and export sums and counts, not averages or percentiles. Consumers can add sums and counts across windows. See [Export a summary table](https://docs.hydrolix.io/latest/exploring-data/export-to-google-pubsub/#export-a-summary-table).

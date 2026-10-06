# Hydrolix to Google Pub/Sub exporter

This program copies rows from a Hydrolix table to a Google Pub/Sub topic. It queries the Hydrolix HTTP query API one time window at a time and publishes each row as one message.

For step-by-step instructions, see [Export Data to Google Pub/Sub](https://docs.hydrolix.io/latest/exploring-data/export-to-google-pubsub/) in the Hydrolix documentation.

## Build

```bash
go build -o hdx-pubsub-export .
./hdx-pubsub-export -h
```

## How it works

- The exporter splits time into windows (`--window`, one minute by default). A window includes rows at its start time and leaves out rows at its end time.
- It waits `--delay` (one minute by default) after a window ends before it queries it. New rows take a short time to become ready to query.
- It publishes each row of the query result as one message. Each message has two attributes, `window_start` and `window_end`, in RFC 3339 format.
- With `--start`, it begins at that time. Windows in the past run right away, then it follows new data. With `--end`, it stops there.
- `--query` replaces the default `SELECT *` with custom SQL. Use `{start}` and `{end}` for the window's boundaries, and leave out `FORMAT`.
- When it stops, from Ctrl+C, SIGTERM, or an error, it logs the `--start` value to resume from. On Ctrl+C or SIGTERM, it finishes the window it's working on first.

## Run it in production

The commands on the docs page are for trying the exporter. Before relying on it in production, check these.

- **Use a service account token**. A login token expires after 24 hours. A [service account token](https://docs.hydrolix.io/latest/self-managed/advanced-configuration/authentication-and-authorization/account-types/service-accounts-howto/#service-account-tokens) lasts 365 days by default, but it also expires. The exporter stops when its token stops working. Give the service account a role that can query only the tables it exports. Pass the token in the `HDX_TOKEN` environment variable, not as `--hdx_token`, which shows in the process list.
- **Give the Google Cloud identity the Pub/Sub Publisher role**. `roles/pubsub.publisher` can publish to a topic. The exporter doesn't create topics. Create the topic ahead of time.
- **Plan for restarts**. The exporter doesn't save its place. Run it under something that restarts it with the `--start` value from its last log line. Without `--start`, it starts one `--delay` before the current time, and rows from the gap aren't exported.
- **Expect duplicates**. If the exporter stops on an error partway through a window, resuming exports that window again. Pub/Sub can also deliver a message more than once. The `window_start` and `window_end` attributes tell a consumer which window a message came from. To remove duplicates, compare row contents.
- **Don't rely on order**. Messages don't carry an ordering key, and Pub/Sub doesn't keep them in order.
- **Set `--delay` for the table**. Rows that arrive after their window is queried aren't exported. In testing on a table with default settings, a 10-second delay missed many new rows, and 30 seconds or more missed none. If rows reach Hydrolix late, raise `--delay` to match.
- **Keep queries fast**. Each window is one query. `--query_timeout` (five minutes by default) limits how long it can take. Select only the needed columns.
- **Mind the message size limit**. Pub/Sub accepts messages up to 10 MB, counting attributes. A row larger than that can't be published, and the exporter stops.
- **Export summary tables with a query**. `SELECT *` returns a summary table's stored partial results, not its values. Use `--query`, and export sums and counts rather than averages or percentiles. Consumers can add sums and counts across minutes.

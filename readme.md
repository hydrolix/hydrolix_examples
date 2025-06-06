This repository was created in order to provide various examples of how to work with Hydrolix. It also serves as repository for other miscellaneous scripts.

View the README.md file in each directory for more information.

* A [demonstration Google Pub/Sub exporter](exporters/http_to_pubsub) for your Hydrolix data
* A [Getting Started Guide](nginx_web_access_logs) with NGINX web access logs to accompany our [Getting Started tutorial](https://docs.hydrolix.io/docs/getting-started-on-hydrolix)
* [Sample transforms](sample_transforms) for:
  * NGINX web access logs
  * Apache web access logs
  * Netflow logs
  * Note that more transforms can be found in the official [Hydrolix Transform Repository](https://github.com/hydrolix/transforms))
* Cribl: [Sample configuration](https://github.com/hydrolix/hydrolix_examples/tree/main/cribl) to export data from Cribl into Hydrolix
* Cloudflare: [Sample transform](https://github.com/hydrolix/hydrolix_examples/tree/main/cloudflare) to structure data exported from Cloudflare into Hydrolix
* Miscellaneous
  * A [Postgres v11 -> v12 upgrade script](miscellaneous/postgres-upgrade-job.yaml) for customers using the internal Postgres pod

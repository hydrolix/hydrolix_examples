# Zuplo Integration with Hydrolix

Use the configuration files in this repo to accompish either of the following:
* Generate an API gateway in Zuplo for a Hydrolix cluster
* Export Zuplo logs to Hydrolix and generate Grafana dashboards to view the exported Zuplo data

## Getting Started

### Dependencies

* [A running Hydrolix cluster](https://docs.hydrolix.io/docs/welcome)
* [A Zuplo account](https://zuplo.com/docs/articles/what-is-zuplo)
* [Hydrolix / Akamai TrafficPeak Plugin](https://zuplo.com/docs/articles/plugin-hydrolix-traffic-peak), required for Zuplo log export to Hydrolix

## Installation

See the Hydrolix [integration with Zuplo documentation](https://docs.hydrolix.io/docs/zuplo-integration) for installation details.

Note that while the [Grafana dashboard JSON files](https://github.com/hydrolix/hydrolix_examples/tree/master/zuplo/grafana-dashboards) and [Hydrolix transform](https://github.com/hydrolix/hydrolix_examples/tree/master/zuplo/transforms) can be used as-is, the [OpenAPI schema](https://github.com/hydrolix/hydrolix_examples/tree/master/zuplo/openapi-schemas) must be edited before use to replace the following line:

```json
servers:
- url: https://{myhost}.hydrolix.live
```

with the URL of your running Hydrolix cluster. For more details, see the [Zuplo integration documentation](https://docs.hydrolix.io/docs/zuplo-integration).

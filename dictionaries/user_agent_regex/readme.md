# User-Agent Regexp Dictionary

A [regexp tree dictionary](https://docs.hydrolix.io/docs/regexp-tree-dictionaries) that categorizes user-agent strings during ingest. It groups high-cardinality user-agent values into a small set of categories such as search engine crawlers, AI crawlers, and desktop browsers, so you can index, group by, and filter on the category instead of the raw string.

The dictionary file is [`user_agent_regex_dict.yaml`](user_agent_regex_dict.yaml).

## Attributes

Each pattern node sets these attributes:

| Attribute | Type | Description |
| --- | --- | --- |
| `regexp` | string | The pattern matched against the user-agent string. This is the dictionary primary key. |
| `ua_category` | string | The category, such as `Search Engine Crawler`, `AI LLM Crawler`, or `Desktop Browser`. |
| `is_bot` | uint8 | `1` for automated traffic, `0` for human traffic. |
| `ai_category` | string | The AI vendor for AI crawler categories, such as `OpenAI`, `Anthropic`, or `Google AI`. |

## Use the dictionary

Create a dictionary from the YAML file with the `regexp_tree` layout and `regexp` primary key. Define the schema with these attributes:

```json
[
  { "name": "regexp",      "datatype": { "type": "string", "denullify": true } },
  { "name": "ua_category", "datatype": { "type": "string", "denullify": true } },
  { "name": "is_bot",      "datatype": { "type": "uint8",  "denullify": true } },
  { "name": "ai_category", "datatype": { "type": "string", "denullify": true } }
]
```

To categorize the user agent at ingest, add a `dictGet` lookup to the table's SQL transform that reads the user-agent column:

```sql
dictGet('myproject_user_agent_regex', 'ua_category', agent) AS user_agent_category
```

For the full walkthrough, see the [Regexp Tree Dictionaries](https://docs.hydrolix.io/docs/regexp-tree-dictionaries) documentation.

## Sources

The category values were seeded from [useragents.me](https://www.useragents.me/) and extended with bot and crawler signatures.

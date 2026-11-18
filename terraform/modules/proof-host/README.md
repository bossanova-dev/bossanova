# proof-host

A Cloudflare host for per-PR proof pages. It creates a private R2 bucket, a Worker that serves the
bucket on your hostname, a daily retention sweep, and an Access application that covers only the
sign-in and publish paths.

- Readers sign in with Access SSO. The Worker checks the Access JWT itself on every page read, from
  the `CF_Authorization` cookie or the `Cf-Access-Jwt-Assertion` header, and admits a login whose
  email domain is in `allowed_email_domains`.
- A framed page with no valid session shows a small sign-in gate that opens sign-in in a new tab.
  Access's own login page refuses to be framed, so it is never shown in a frame. A top-level tab is
  redirected to `/_signin` instead.
- Uploads are `PUT /_publish/<owner>/<repo>/<pr>/<sha>/<name>` with the module's Access service
  token. No other identity can write.
- Pages may be framed only by the origins in `frame_ancestors`. A page's `player.js` is the only
  stored object served as a script.
- Once a day the Worker deletes every object uploaded more than `retention_days` days ago.

## Usage

Pin the module to a commit of this repository:

```hcl
module "proof_host" {
  source = "github.com/bossanova-dev/bossanova//terraform/modules/proof-host?ref=<commit sha>"

  account_id            = var.cloudflare_account_id
  zone_id               = var.cloudflare_zone_id
  hostname              = "proof.example.com"
  bucket_name           = "example-proof-pages"
  access_team_domain    = "example.cloudflareaccess.com"
  allowed_email_domains = ["example.com"]
}

output "proof_base_url" {
  value = module.proof_host.base_url
}

output "proof_publish_client_id" {
  value = module.proof_host.publish_client_id
}

output "proof_publish_client_secret" {
  sensitive = true
  value     = module.proof_host.publish_client_secret
}
```

The Cloudflare provider's API token needs edit access to Workers, R2, Zero Trust Access and the
zone's DNS.

## Inputs

| Name                    | Type           | Default                                                              | Description                                                                           |
| ----------------------- | -------------- | -------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| `account_id`            | `string`       | required                                                             | Cloudflare account ID.                                                                |
| `zone_id`               | `string`       | required                                                             | Zone ID of the zone that holds `hostname`.                                            |
| `hostname`              | `string`       | required                                                             | Hostname the pages are served on. The Worker custom domain creates its DNS record.    |
| `bucket_name`           | `string`       | required                                                             | Name of the private R2 bucket.                                                        |
| `location`              | `string`       | `null`                                                               | R2 location hint (`APAC`, `EEUR`, `ENAM`, `WEUR`, `WNAM`). Null lets Cloudflare pick. |
| `access_team_domain`    | `string`       | required                                                             | Access team domain without a scheme, such as `example.cloudflareaccess.com`.          |
| `allowed_email_domains` | `list(string)` | required                                                             | Email domains whose SSO logins may read pages. At least one.                          |
| `identity_provider_ids` | `list(string)` | `[]`                                                                 | Access identity providers allowed at sign-in. Empty allows all of the account's.      |
| `session_duration`      | `string`       | `"24h"`                                                              | Access session length.                                                                |
| `frame_ancestors`       | `list(string)` | `["https://app.bossanova.dev", "https://app-staging.bossanova.dev"]` | HTTPS origins that may frame a page. Empty forbids framing.                           |
| `retention_days`        | `number`       | `90`                                                                 | Days an object is kept before the daily sweep deletes it. A positive integer.         |
| `name`                  | `string`       | `null`                                                               | Worker name and prefix of the Access names. Null uses `bucket_name`.                  |

## Outputs

| Name                    | Description                                                               |
| ----------------------- | ------------------------------------------------------------------------- |
| `base_url`              | `https://<hostname>`, the base URL a publisher uploads to and links from. |
| `bucket_name`           | Name of the R2 bucket.                                                    |
| `access_aud`            | AUD tag of the Access application. Not a secret.                          |
| `publish_client_id`     | Client ID of the publish service token.                                   |
| `publish_client_secret` | Client secret of the publish service token. Sensitive.                    |

## Storing the publish token

The publisher sends the service token as `CF-Access-Client-Id` and `CF-Access-Client-Secret`. On a
macOS host, store both in the login keychain. Pass `-w` last with no value so `security` prompts
for it, and no secret lands in shell history:

```sh
terraform output -raw proof_publish_client_id
security add-generic-password -U -s <service> -a PROOF_ACCESS_CLIENT_ID -w

terraform output -raw proof_publish_client_secret
security add-generic-password -U -s <service> -a PROOF_ACCESS_CLIENT_SECRET -w
```

Paste each value at the prompt. To rotate the token, replace the service token resource
(`terraform apply -replace=module.proof_host.cloudflare_zero_trust_access_service_token.publish`)
and store the new pair the same way.

## Limits

- A browser that blocks third-party cookies (Safari by default) never sends `CF_Authorization` to a
  framed page, so the frame keeps showing the gate after sign-in. The page still opens in its own
  tab.
- Provider 4.x cannot turn off the Worker's `workers.dev` route, so the Worker checks every token
  itself, on the Access-covered paths too.

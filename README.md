# Kube Headscale

## What

kube-headscale is a sidecar and helm chart to make deploying and using headscale in kubernetes easy.

## How

In addition to deploying headscale itself, the sidecar will watch for specially labeled ConfigMaps and Secrets in order to dynamically manage the policy, derp maps, and DNS records, provision and rotate ApiKeys, and provison PreAuth Keys.

## Usage

Deploy the chart using helm

```bash
helm install ghcr.io/meln5674/kube-headscale/chart headscale [-f my-values.yaml]
```

See [here](./deploy/helm/kube-headscale/values.yaml) for documentation on available fields.

### Policy

To modify the policy, create a ConfigMap with the label `kube-headscale.meln5674.io/policy` in the same namespace as the chart. All keys of all ConfigMaps with this label will be concatenated together to form a single policy.json file and dynamically updated through the api. Keys will be parsed as YAML and automatically converted to JSON.

### DERP Map

To modify the DERP map, create a ConfigMap with the label `kube-headscale.meln5674.io/derpmap` in the same namespace as the chart. All keys of all ConfigMaps with this label will be concatenated together and served to headscale as the "Regions" fields over a localhost port whose URL will automatically be injected into the config at startup. Keys will be parsed as YAML.

### DNS Records

To modify the DNS records, create a ConfigMap with the label `kube-headscale.meln5674.io/dns-extra-records` in the same namespace as the chart. All keys of all ConfigMaps with this label will be concatenated together and served to headscale via the `dns.extra_records_path` config setting, which will be automatically injected on startup. Keys will be parsed as YAML and automatically converted to JSON.

### API Keys

To generate an API key, create a Secret with the label `kube-headscale.meln5674.io/apikey` in the samenamespace as the chart. Any Secret with this label will cause an ApiKey to be generated and stored in the key `apikey` of the secret. To set a specific expiration, set the annotation `kube-headscale.meln5674.io/apikey-expiration` to a ISO8601 date. To set a expriation duration and automatically rotate the key after expiration, set the annotation `kube-headscale.meln5674.io/apikey-lifetime` instead. If neither are set, a 90 day expiration will be set, matching the CLI default behavior.

To manually rotate the key, delete the `apikey` field of the secret or set it to an empty string.

### PreAuth Keys

To generate a PreAuth key, create a Secret with the label `kube-headscale.meln5674.io/preauthkey=USER` in the same namespace as the chart. Any Secret with this label will cause a PreAuth Key to be generated and stored in the key `preauthkey` of the secret.

To manually rotate the key, delete the `preauthkey` field of the secret or set it to an empty string.
